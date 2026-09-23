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
	"sync/atomic"
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
	Connected() bool
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

	mu        sync.Mutex
	configMu  sync.RWMutex
	started   bool
	closed    bool
	startedAt *time.Time

	// telemetryMu guards lastTelemetry - data-plane state updated on every
	// accepted message, deliberately separate from configMu (control-plane
	// policy) so a burst of telemetry never contends with Apply/Devices.
	telemetryMu   sync.RWMutex
	lastTelemetry map[string]telemetryEntry

	// eventsMu guards recentEvents and nextEventSeq - same reasoning as
	// telemetryMu: a data-plane ring buffer, separate from configMu.
	eventsMu     sync.RWMutex
	recentEvents []ActivityEvent
	nextEventSeq uint64

	acceptedMessages     atomic.Uint64
	rejectedMessages     atomic.Uint64
	localRoutesPublished atomic.Uint64
	localRoutesFailed    atomic.Uint64
	outboxStored         atomic.Uint64
	outboxDiscarded      atomic.Uint64
	outboxFailed         atomic.Uint64
}

// Snapshot is the safe, payload-free runtime state used by local diagnostics.
// Counters are monotonic from gateway construction until process shutdown.
type Snapshot struct {
	StartedAt            *time.Time
	Started              bool
	MQTTConnected        bool
	Subscriptions        int
	AcceptedMessages     uint64
	RejectedMessages     uint64
	LocalRoutesPublished uint64
	LocalRoutesFailed    uint64
	OutboxStored         uint64
	OutboxDiscarded      uint64
	OutboxFailed         uint64
}

type route struct {
	deviceID string
	kind     Kind
}

// telemetryEntry is the most recently accepted Telemetry-kind payload for
// one device, kept only in memory - no history, no persistence. See
// LastTelemetry's doc comment.
type telemetryEntry struct {
	payload    []byte
	observedAt time.Time
}

// maxRecentEvents bounds the in-memory activity ring buffer (RecentEvents)
// the same way outbox/registry tables are bounded on disk - an edge device
// principle this codebase already applies everywhere state accumulates.
const maxRecentEvents = 200

// defaultEventsLimit is how many events RecentEvents returns per call when
// EventFilter.Limit is unset - a page for gateway-web's activity table, not
// the whole buffer every time.
const defaultEventsLimit = 50

// ActivityEvent is one payload-free entry in the in-memory activity log -
// same privacy discipline as Message/RejectedMessage above (never a
// payload), kept only for RecentEvents/GetRecentEvents (docs/api-v1.md).
// No history beyond maxRecentEvents, no persistence - a restart forgets it,
// like every other counter on Gateway.
type ActivityEvent struct {
	// Sequence is monotonically increasing, assigned by recordEvent. It is
	// a stable identifier/cursor - gateway-web uses it as a React key and
	// as EventFilter.BeforeSequence's pagination cursor.
	Sequence  uint64
	Timestamp time.Time
	DeviceID  string
	// Kind is empty for a route_published/route_failed outcome - a route
	// spans two devices, not one.
	Kind  Kind
	Topic string
	// Outcome is one of "accepted", "rejected", "route_published",
	// "route_failed".
	Outcome string
	// Detail is the rejection reason, or the route ID (plus failure cause
	// for route_failed) - never a payload.
	Detail string
}

// EventFilter narrows RecentEvents. The zero value means "the most recent
// defaultEventsLimit events, any device, no time bound".
type EventFilter struct {
	// DeviceID, if non-empty, only returns events for that device.
	DeviceID string
	// Since, if non-zero, excludes events older than it.
	Since time.Time
	// Limit caps the page size; 0 means defaultEventsLimit. Always capped
	// at maxRecentEvents regardless of what is requested.
	Limit int
	// BeforeSequence, if non-zero, only considers events with a strictly
	// smaller Sequence - gateway-web's "next page" cursor.
	BeforeSequence uint64
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
		client:        client,
		logger:        logger,
		outbox:        queue,
		lastTelemetry: make(map[string]telemetryEntry),
	}
	routes, forwards, toOutbox, topics, devices, err := buildRouting(cfg, queue, requireOutbox)
	if err != nil {
		return nil, err
	}
	gateway.routes, gateway.forwards, gateway.toOutbox, gateway.topics, gateway.devices = routes, forwards, toOutbox, topics, devices
	return gateway, nil
}

// buildRouting creates a complete policy snapshot before it becomes visible
// to message callbacks. Apply uses the same validation path as construction.
func buildRouting(cfg config.Config, queue Outbox, requireOutbox bool) (map[string]route, map[string][]config.Route, map[string]outbox.Kind, []string, map[string]config.Device, error) {
	routes := make(map[string]route)
	forwards := make(map[string][]config.Route)
	toOutbox := make(map[string]outbox.Kind)
	devices := make(map[string]config.Device)
	var topics []string
	for _, device := range cfg.Devices {
		devices[device.ID] = device
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
			if existing, exists := routes[candidate.topic]; exists {
				return nil, nil, nil, nil, nil, fmt.Errorf("MQTT topic %q is assigned to both %s and %s", candidate.topic, existing.deviceID, device.ID)
			}
			routes[candidate.topic] = route{deviceID: device.ID, kind: candidate.kind}
			topics = append(topics, candidate.topic)
			if forwardsToVPS(device, candidate.kind) {
				if queue == nil && requireOutbox {
					return nil, nil, nil, nil, nil, errors.New("outbox is required when forwarding to VPS is enabled")
				}
				if queue != nil {
					toOutbox[candidate.topic] = outbox.Kind(candidate.kind)
				}
			}
		}
	}
	for _, forward := range cfg.Routes {
		forwards[forward.SourceTopic] = append(forwards[forward.SourceTopic], forward)
	}
	sort.Strings(topics)
	return routes, forwards, toOutbox, topics, devices, nil
}

// Snapshot returns transport state and counters without device payloads,
// credentials, topic names, or identifiers.
func (g *Gateway) Snapshot() Snapshot {
	g.mu.Lock()
	started := g.started
	startedAt := g.startedAt
	g.mu.Unlock()
	g.configMu.RLock()
	subscriptions := 0
	if started {
		subscriptions = len(g.topics)
	}
	g.configMu.RUnlock()
	return Snapshot{
		StartedAt:            startedAt,
		Started:              started,
		MQTTConnected:        g.client.Connected(),
		Subscriptions:        subscriptions,
		AcceptedMessages:     g.acceptedMessages.Load(),
		RejectedMessages:     g.rejectedMessages.Load(),
		LocalRoutesPublished: g.localRoutesPublished.Load(),
		LocalRoutesFailed:    g.localRoutesFailed.Load(),
		OutboxStored:         g.outboxStored.Load(),
		OutboxDiscarded:      g.outboxDiscarded.Load(),
		OutboxFailed:         g.outboxFailed.Load(),
	}
}

// LastTelemetry returns the most recently accepted Telemetry-kind payload
// for deviceID, and the timestamp the payload itself declared (already
// validated RFC3339 UTC by validateInbound) - not when this method was
// called. ok is false when nothing has been received since the process
// started, which is not an error: the caller (internal/api) turns that into
// available=false, not a failure. There is no history and nothing is
// persisted - a restart forgets it, same as every other counter on Gateway.
func (g *Gateway) LastTelemetry(deviceID string) ([]byte, time.Time, bool) {
	g.telemetryMu.RLock()
	defer g.telemetryMu.RUnlock()
	entry, ok := g.lastTelemetry[deviceID]
	if !ok {
		return nil, time.Time{}, false
	}
	return append([]byte(nil), entry.payload...), entry.observedAt, true
}

// RecentEvents returns a filtered page of the in-memory activity ring
// buffer, most recent first, plus whether older matching events exist
// beyond the page (hasMore) - see ActivityEvent's doc comment for what it
// captures and why (no payload, no persistence, capped at maxRecentEvents),
// and EventFilter's for what each field does.
//
// recentEvents is stored oldest-first internally (append-friendly); this
// walks it backward so both the "most recent first" ordering and the
// Since/BeforeSequence cutoffs are natural single-pass checks.
func (g *Gateway) RecentEvents(filter EventFilter) (events []ActivityEvent, hasMore bool) {
	g.eventsMu.RLock()
	defer g.eventsMu.RUnlock()
	limit := filter.Limit
	if limit <= 0 || limit > maxRecentEvents {
		limit = defaultEventsLimit
	}
	events = make([]ActivityEvent, 0, limit)
	for i := len(g.recentEvents) - 1; i >= 0; i-- {
		event := g.recentEvents[i]
		if filter.BeforeSequence != 0 && event.Sequence >= filter.BeforeSequence {
			continue
		}
		// recentEvents is strictly ordered by Sequence/Timestamp, so once
		// one event is older than Since, every event before it (older
		// still) is too - safe to stop rather than merely skip it.
		if !filter.Since.IsZero() && event.Timestamp.Before(filter.Since) {
			break
		}
		if filter.DeviceID != "" && event.DeviceID != filter.DeviceID {
			continue
		}
		if len(events) >= limit {
			hasMore = true
			break
		}
		events = append(events, event)
	}
	return events, hasMore
}

// recordEvent appends to the ring buffer, dropping the oldest entry once
// maxRecentEvents is reached, and assigns the next monotonic Sequence.
func (g *Gateway) recordEvent(event ActivityEvent) {
	g.eventsMu.Lock()
	defer g.eventsMu.Unlock()
	g.nextEventSeq++
	event.Sequence = g.nextEventSeq
	g.recentEvents = append(g.recentEvents, event)
	if excess := len(g.recentEvents) - maxRecentEvents; excess > 0 {
		g.recentEvents = g.recentEvents[excess:]
	}
}

// Devices returns a stable copy of the currently applied device policy. It
// lets the local API describe the same revision that command publishing uses.
func (g *Gateway) Devices() []config.Device {
	g.configMu.RLock()
	devices := make([]config.Device, 0, len(g.devices))
	for _, device := range g.devices {
		devices = append(devices, device)
	}
	g.configMu.RUnlock()
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices
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
	g.configMu.RLock()
	topics := append([]string(nil), g.topics...)
	g.configMu.RUnlock()
	for _, topic := range topics {
		if err := g.client.Subscribe(ctx, topic, g.handleMessage); err != nil {
			g.client.Close()
			return fmt.Errorf("subscribe to %q: %w", topic, err)
		}
	}
	startedAt := time.Now().UTC()
	g.startedAt = &startedAt
	g.started = true
	return nil
}

// Apply replaces the routing policy without restarting the MQTT client.
// New subscriptions are installed before the new policy becomes visible;
// removed topics are ignored immediately and are unsubscribed afterwards
// when the transport supports it.
func (g *Gateway) Apply(ctx context.Context, cfg config.Config) error {
	routes, forwards, toOutbox, topics, devices, err := buildRouting(cfg, g.outbox, true)
	if err != nil {
		return err
	}
	g.mu.Lock()
	started, closed := g.started, g.closed
	g.mu.Unlock()
	if closed {
		return errors.New("MQTT gateway is closed")
	}
	g.configMu.Lock()
	oldTopics := append([]string(nil), g.topics...)
	oldSet := make(map[string]struct{}, len(oldTopics))
	for _, topic := range oldTopics {
		oldSet[topic] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		newSet[topic] = struct{}{}
	}
	if started {
		added := make([]string, 0)
		for _, topic := range topics {
			if _, exists := oldSet[topic]; exists {
				continue
			}
			if err := g.client.Subscribe(ctx, topic, g.handleMessage); err != nil {
				if client, ok := g.client.(interface {
					Unsubscribe(context.Context, string) error
				}); ok {
					for _, rollback := range added {
						_ = client.Unsubscribe(context.Background(), rollback)
					}
				}
				g.configMu.Unlock()
				return fmt.Errorf("subscribe to %q while applying registry revision: %w", topic, err)
			}
			added = append(added, topic)
		}
	}
	g.routes, g.forwards, g.toOutbox, g.topics, g.devices = routes, forwards, toOutbox, topics, devices
	g.configMu.Unlock()
	// A device no longer in the new policy (disabled or removed) must not
	// keep showing a stale last-known reading forever.
	g.telemetryMu.Lock()
	for id := range g.lastTelemetry {
		if _, exists := devices[id]; !exists {
			delete(g.lastTelemetry, id)
		}
	}
	g.telemetryMu.Unlock()
	if started {
		if client, ok := g.client.(interface {
			Unsubscribe(context.Context, string) error
		}); ok {
			for _, topic := range oldTopics {
				if _, exists := newSet[topic]; !exists {
					_ = client.Unsubscribe(ctx, topic)
				}
			}
		}
	}
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
	g.configMu.RLock()
	device, ok := g.devices[deviceID]
	g.configMu.RUnlock()
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
	g.configMu.RLock()
	route, exists := g.routes[topic]
	if !exists {
		g.configMu.RUnlock()
		return
	}
	toOutbox, forward := g.toOutbox[topic]
	forwards := append([]config.Route(nil), g.forwards[topic]...)
	g.configMu.RUnlock()
	message, err := validateInbound(route, topic, payload)
	if err != nil {
		g.rejectedMessages.Add(1)
		g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: err.Error()})
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Outcome: "rejected", Detail: err.Error()})
		return
	}
	g.acceptedMessages.Add(1)
	g.logger.Accepted(ctx, message)
	g.recordEvent(ActivityEvent{Timestamp: message.Timestamp, DeviceID: message.DeviceID, Kind: message.Kind, Topic: message.Topic, Outcome: "accepted"})
	if route.kind == Telemetry {
		g.telemetryMu.Lock()
		g.lastTelemetry[route.deviceID] = telemetryEntry{payload: message.Payload, observedAt: message.Timestamp}
		g.telemetryMu.Unlock()
	}
	if kind, forward := toOutbox, forward; forward {
		result, err := g.outbox.Enqueue(ctx, outbox.Message{
			MessageID: message.MessageID,
			DeviceID:  message.DeviceID,
			Topic:     message.Topic,
			Kind:      kind,
			Payload:   message.Payload,
		})
		switch {
		case err != nil:
			g.outboxFailed.Add(1)
		case result.Stored:
			g.outboxStored.Add(1)
		default:
			g.outboxDiscarded.Add(1)
		}
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
	for _, forward := range forwards {
		payload, err := transformJSONCommand(forward.Transform.CommandType, message.Payload)
		if err == nil {
			err = g.client.Publish(ctx, forward.DestinationTopic, payload, forward.QoS, forward.Retain)
		}
		if err != nil {
			g.localRoutesFailed.Add(1)
			g.rejectedMessages.Add(1)
			g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: fmt.Sprintf("route %s failed: %v", forward.ID, err)})
			g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Topic: forward.DestinationTopic, Outcome: "route_failed", Detail: forward.ID + ": " + err.Error()})
		} else {
			g.localRoutesPublished.Add(1)
			g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Topic: forward.DestinationTopic, Outcome: "route_published", Detail: forward.ID})
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
