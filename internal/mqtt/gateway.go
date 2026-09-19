// Package mqtt contains the gateway's MQTT-facing application logic.
package mqtt

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

const qosAtLeastOnce byte = 1

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Kind identifies the semantic purpose of an MQTT topic.
type Kind string

const (
	Telemetry     Kind = "telemetry"
	State         Kind = "state"
	Event         Kind = "event"
	CommandResult Kind = "command-result"
)

// Message is a validated message received from a device. Payload is retained
// for the next pipeline stage, but the default logger deliberately never logs it.
type Message struct {
	DeviceID  string
	Kind      Kind
	Topic     string
	MessageID string
	Timestamp time.Time
	Payload   []byte
}

// RejectedMessage describes a message that was not accepted. It deliberately
// has no payload, because device data must not be leaked into logs by default.
type RejectedMessage struct {
	DeviceID string
	Kind     Kind
	Topic    string
	Reason   string
}

// Logger receives the two operationally useful message outcomes.
type Logger interface {
	Accepted(context.Context, Message)
	Rejected(context.Context, RejectedMessage)
}

// OutboxLogger is an optional operational logging extension. Its methods
// never receive a payload, keeping device data out of default logs.
type OutboxLogger interface {
	OutboxStored(context.Context, Message, outbox.EnqueueResult)
	OutboxDiscarded(context.Context, Message, outbox.EnqueueResult)
	OutboxFailed(context.Context, Message, error)
}

// MessageHandler is invoked by a Client for a message on a subscribed topic.
type MessageHandler func(ctx context.Context, topic string, payload []byte)

// Client is the small MQTT transport boundary used by the gateway. It keeps
// gateway tests independent of a live Mosquitto broker and MQTT library.
type Client interface {
	Connect(context.Context) error
	Subscribe(context.Context, string, MessageHandler) error
	Publish(context.Context, string, []byte, byte, bool) error
	Close()
}

// Outbox is the minimal durable forwarding boundary used by the gateway.
// The SQLite implementation lives in internal/outbox and has no MQTT imports.
type Outbox interface {
	Enqueue(context.Context, outbox.Message) (outbox.EnqueueResult, error)
}

// Credentials are read separately from YAML so configuration can be committed
// without secrets.
type Credentials struct {
	Username string
	Password string
}

// ResolveCredentials reads the required configured environment variables.
func ResolveCredentials(mqtt config.MQTT, lookup func(string) string) (Credentials, error) {
	if lookup == nil {
		return Credentials{}, errors.New("environment lookup is required")
	}
	usernameEnv := strings.TrimSpace(mqtt.UsernameEnv)
	passwordEnv := strings.TrimSpace(mqtt.PasswordEnv)
	if usernameEnv == "" || passwordEnv == "" {
		return Credentials{}, errors.New("mqtt.username_env and mqtt.password_env must be configured together")
	}
	username := lookup(usernameEnv)
	if username == "" {
		return Credentials{}, fmt.Errorf("%s is not set", usernameEnv)
	}
	password := lookup(passwordEnv)
	if password == "" {
		return Credentials{}, fmt.Errorf("%s is not set", passwordEnv)
	}
	return Credentials{Username: username, Password: password}, nil
}

// Gateway subscribes to configured device topics and publishes validated commands.
type Gateway struct {
	client   Client
	logger   Logger
	routes   map[string]route
	forwards map[string][]config.Route
	outbox   Outbox
	toOutbox map[string]outbox.Kind
	topics   []string
	devices  map[string]config.Device

	mu      sync.Mutex
	started bool
	closed  bool
}

type route struct {
	deviceID string
	kind     Kind
}

// New constructs the MQTT gateway from an already validated configuration.
func New(cfg config.Config, client Client, logger Logger, queue Outbox) (*Gateway, error) {
	return newGateway(cfg, client, logger, queue, true)
}

// NewCommandPublisher constructs the narrow gateway capability needed by the
// publish-test-command CLI. It intentionally does not open or require the
// durable outbox, because it cannot receive and forward inbound messages.
func NewCommandPublisher(cfg config.Config, client Client, logger Logger) (*Gateway, error) {
	return newGateway(cfg, client, logger, nil, false)
}

func newGateway(cfg config.Config, client Client, logger Logger, queue Outbox, requireOutbox bool) (*Gateway, error) {
	if client == nil {
		return nil, errors.New("MQTT client is required")
	}
	if logger == nil {
		return nil, errors.New("MQTT logger is required")
	}

	gateway := &Gateway{
		client:   client,
		logger:   logger,
		routes:   make(map[string]route),
		forwards: make(map[string][]config.Route),
		outbox:   queue,
		toOutbox: make(map[string]outbox.Kind),
		devices:  make(map[string]config.Device),
	}
	for _, device := range cfg.Devices {
		gateway.devices[device.ID] = device
		if device.Enabled == nil || !*device.Enabled {
			continue
		}
		for _, candidate := range []struct {
			topic string
			kind  Kind
		}{
			{device.Topics.Telemetry, Telemetry},
			{device.Topics.State, State},
			{device.Topics.Event, Event},
			{device.Topics.CommandResult, CommandResult},
		} {
			if candidate.topic == "" {
				continue
			}
			if existing, exists := gateway.routes[candidate.topic]; exists {
				return nil, fmt.Errorf("MQTT topic %q is assigned to both %s and %s", candidate.topic, existing.deviceID, device.ID)
			}
			gateway.routes[candidate.topic] = route{deviceID: device.ID, kind: candidate.kind}
			gateway.topics = append(gateway.topics, candidate.topic)
			if forwardsToVPS(device, candidate.kind) {
				if queue == nil && requireOutbox {
					return nil, errors.New("outbox is required when forwarding to VPS is enabled")
				}
				if queue != nil {
					gateway.toOutbox[candidate.topic] = outbox.Kind(candidate.kind)
				}
			}
		}
	}
	for _, forward := range cfg.Routes {
		gateway.forwards[forward.SourceTopic] = append(gateway.forwards[forward.SourceTopic], forward)
	}
	sort.Strings(gateway.topics)
	return gateway, nil
}

// Start connects to the broker and subscribes to every inbound topic for
// enabled devices. A partial startup is closed so the caller can retry cleanly.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("MQTT gateway is closed")
	}
	if g.started {
		return errors.New("MQTT gateway has already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.client.Connect(ctx); err != nil {
		g.client.Close()
		return fmt.Errorf("connect MQTT client: %w", err)
	}
	for _, topic := range g.topics {
		if err := g.client.Subscribe(ctx, topic, g.handleMessage); err != nil {
			g.client.Close()
			return fmt.Errorf("subscribe to %q: %w", topic, err)
		}
	}
	g.started = true
	return nil
}

// Close releases the underlying MQTT connection. It is safe to call more than once.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.client.Close()
	g.closed = true
}

// PublishCommand validates and publishes an outbound command at QoS 1 without
// retaining it. Commands are only allowed for enabled, configured devices.
func (g *Gateway) PublishCommand(ctx context.Context, deviceID string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	device, ok := g.devices[deviceID]
	if !ok {
		return fmt.Errorf("unknown device %q", deviceID)
	}
	if device.Enabled == nil || !*device.Enabled {
		return fmt.Errorf("device %q is disabled", deviceID)
	}
	if device.Topics.Command == "" {
		return fmt.Errorf("device %q does not define a command topic", deviceID)
	}
	if err := validateCommand(payload); err != nil {
		return fmt.Errorf("invalid command for device %q: %w", deviceID, err)
	}
	if err := g.client.Publish(ctx, device.Topics.Command, append([]byte(nil), payload...), qosAtLeastOnce, false); err != nil {
		return fmt.Errorf("publish command to %q: %w", deviceID, err)
	}
	return nil
}

func (g *Gateway) handleMessage(ctx context.Context, topic string, payload []byte) {
	route, exists := g.routes[topic]
	if !exists {
		return
	}
	message, err := validateInbound(route, topic, payload)
	if err != nil {
		g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: err.Error()})
		return
	}
	g.logger.Accepted(ctx, message)
	if kind, forward := g.toOutbox[topic]; forward {
		result, err := g.outbox.Enqueue(ctx, outbox.Message{
			MessageID: message.MessageID,
			DeviceID:  message.DeviceID,
			Topic:     message.Topic,
			Kind:      kind,
			Payload:   message.Payload,
		})
		if logger, ok := g.logger.(OutboxLogger); ok {
			switch {
			case err != nil:
				logger.OutboxFailed(ctx, message, err)
			case result.Stored:
				logger.OutboxStored(ctx, message, result)
			default:
				logger.OutboxDiscarded(ctx, message, result)
			}
		}
	}
	for _, forward := range g.forwards[topic] {
		payload, err := transformJSONCommand(forward.Transform.CommandType, message.Payload)
		if err == nil {
			err = g.client.Publish(ctx, forward.DestinationTopic, payload, forward.QoS, forward.Retain)
		}
		if err != nil {
			g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: fmt.Sprintf("route %s failed: %v", forward.ID, err)})
		}
	}
}

func forwardsToVPS(device config.Device, kind Kind) bool {
	switch kind {
	case Telemetry:
		return device.Forwarding.TelemetryToVPS
	case State:
		return device.Forwarding.StateToVPS
	case Event:
		return device.Forwarding.EventsToVPS
	default:
		return false
	}
}

func transformJSONCommand(commandType string, parameters []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(parameters, &object); err != nil || object == nil {
		return nil, errors.New("route payload must be a JSON object")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return json.Marshal(struct {
		CommandID  string          `json:"command_id"`
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}{fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), commandType, parameters})
}

func validateInbound(route route, topic string, payload []byte) (Message, error) {
	if !utf8.Valid(payload) {
		return Message{}, errors.New("payload is not valid UTF-8")
	}
	var envelope struct {
		MessageID string `json:"message_id"`
		Timestamp string `json:"timestamp"`
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		if err != nil {
			return Message{}, fmt.Errorf("payload is not a JSON object: %w", err)
		}
		return Message{}, errors.New("payload must be a JSON object")
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Message{}, fmt.Errorf("decode payload metadata: %w", err)
	}
	if !uuidPattern.MatchString(envelope.MessageID) {
		return Message{}, errors.New("message_id must be a UUID")
	}
	if !strings.HasSuffix(envelope.Timestamp, "Z") {
		return Message{}, errors.New("timestamp must use UTC (Z)")
	}
	timestamp, err := time.Parse(time.RFC3339, envelope.Timestamp)
	if err != nil {
		return Message{}, fmt.Errorf("timestamp must be RFC3339: %w", err)
	}
	return Message{
		DeviceID:  route.deviceID,
		Kind:      route.kind,
		Topic:     topic,
		MessageID: envelope.MessageID,
		Timestamp: timestamp,
		Payload:   append([]byte(nil), payload...),
	}, nil
}

func validateCommand(payload []byte) error {
	if !utf8.Valid(payload) {
		return errors.New("payload is not valid UTF-8")
	}
	var command struct {
		CommandID  string          `json:"command_id"`
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	var parameters map[string]json.RawMessage
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return errors.New("payload must be a JSON object")
	}
	if err := json.Unmarshal(payload, &command); err != nil {
		return fmt.Errorf("decode command: %w", err)
	}
	if !uuidPattern.MatchString(command.CommandID) {
		return errors.New("command_id must be a UUID")
	}
	if strings.TrimSpace(command.Type) == "" {
		return errors.New("type is required")
	}
	if len(command.Parameters) == 0 || json.Unmarshal(command.Parameters, &parameters) != nil || parameters == nil {
		return errors.New("parameters must be a JSON object")
	}
	return nil
}
