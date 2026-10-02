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

	"github.com/ricardossiqueira/iot-gateway/internal/automationrule"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
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
	// Retained is true when the broker delivered a retained replay. It is
	// meaningful to v2 state triggers, which must explicitly opt out of it.
	Retained bool
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
// retained is the MQTT retained flag as the broker delivered it - true for
// a message replayed on (re)subscribe (the broker's last-known value for
// that topic), not necessarily a live transition.
type MessageHandler func(ctx context.Context, topic string, payload []byte, retained bool)

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

// AutomationRecorder persists the event/causation/command-result trail
// Marco 5's automations groundwork needs (docs/device-manifests.md), in
// registry.Store's SQLite, and (item 4) lets the gateway match and rate-
// limit automation rules against an accepted event without caching them
// itself - events are low-frequency enough that a direct query per event
// stays cheap. Optional: a nil recorder simply skips both persistence and
// rule execution, same shape as Outbox being nil in NewCommandPublisher.
type AutomationRecorder interface {
	RecordAutomationEvent(context.Context, registry.AutomationEvent) error
	RecordAutomationCommand(context.Context, registry.AutomationCommand) error
	RecordAutomationCommandResult(context.Context, registry.AutomationCommandResult) (bool, error)
	MatchAutomationRules(ctx context.Context, deviceID, eventType string) ([]registry.AutomationRule, error)
	AutomationCommandExistsForRuleAndCausation(ctx context.Context, ruleID, causationMessageID string) (bool, error)
	CountAutomationCommandsForRuleSince(ctx context.Context, ruleID string, since time.Time) (int, error)
	// GetAutomationRule looks up a single rule by id, for TestAutomationRule.
	GetAutomationRule(ctx context.Context, id string) (registry.AutomationRule, error)
}

// AutomationLogger is an optional operational logging extension for
// AutomationRecorder failures - these are always best-effort and never
// affect message acceptance or command publication, so a Logger that
// doesn't implement this simply has nothing surface them.
type AutomationLogger interface {
	AutomationRecordFailed(ctx context.Context, kind string, err error)
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
	client             Client
	logger             Logger
	routes             map[string]route
	forwards           map[string][]config.Route
	outbox             Outbox
	toOutbox           map[string]outbox.Kind
	topics             []string
	devices            map[string]config.Device
	automationRecorder AutomationRecorder

	// v2Routes is a separately-derived policy: it never reads config.Device
	// nor the legacy automation tables. EnableV2Runtime refreshes this snapshot
	// from active v2 manifest bindings.
	v2Store   V2AutomationStore
	v2Routes  map[string]v2Route
	v2Topics  []string
	v2Devices map[string]devicev2.Manifest

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

// ruleFiringWindow and maxRuleFiringsPerRule bound how often a single
// automation rule (Marco 5 item 4) may fire, standing in for true cycle
// detection: the protocol gives no way to prove an event was itself caused
// by an earlier command (only command-result optionally carries a
// command_id, and no firmware sends it yet), so a per-rule firing-rate
// limit is what's actually enforceable today. Not configurable per rule
// yet - a fixed constant, same posture as automationActivityRetention.
const (
	ruleFiringWindow      = 10 * time.Second
	maxRuleFiringsPerRule = 5
)

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
// recorder may be nil - see AutomationRecorder's doc comment.
func New(cfg config.Config, client Client, logger Logger, queue Outbox, recorder AutomationRecorder) (*Gateway, error) {
	return newGateway(cfg, client, logger, queue, true, recorder)
}

// NewCommandPublisher constructs the narrow gateway capability needed by the
// publish-test-command CLI. It intentionally does not open or require the
// durable outbox, because it cannot receive and forward inbound messages -
// nor does it record automation activity, for the same reason.
func NewCommandPublisher(cfg config.Config, client Client, logger Logger) (*Gateway, error) {
	return newGateway(cfg, client, logger, nil, false, nil)
}

func newGateway(cfg config.Config, client Client, logger Logger, queue Outbox, requireOutbox bool, recorder AutomationRecorder) (*Gateway, error) {
	if client == nil {
		return nil, errors.New("MQTT client is required")
	}
	if logger == nil {
		return nil, errors.New("MQTT logger is required")
	}

	gateway := &Gateway{
		client:             client,
		logger:             logger,
		outbox:             queue,
		automationRecorder: recorder,
		lastTelemetry:      make(map[string]telemetryEntry),
		v2Routes:           make(map[string]v2Route),
		v2Devices:          make(map[string]devicev2.Manifest),
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
	g.configMu.RLock()
	v2Topics := append([]string(nil), g.v2Topics...)
	g.configMu.RUnlock()
	for _, topic := range v2Topics {
		if err := g.client.Subscribe(ctx, topic, g.handleV2Message); err != nil {
			g.client.Close()
			return fmt.Errorf("subscribe to v2 %q: %w", topic, err)
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
	commandID, err := validateCommand(payload)
	if err != nil {
		return fmt.Errorf("invalid command for device %q: %w", deviceID, err)
	}
	if err := g.client.Publish(ctx, device.Topics.Command, append([]byte(nil), payload...), qosAtLeastOnce, false); err != nil {
		return fmt.Errorf("publish command to %q: %w", deviceID, err)
	}
	g.recordAutomationCommand(ctx, registry.AutomationCommand{
		CommandID:   commandID,
		DeviceID:    deviceID,
		Topic:       device.Topics.Command,
		PublishedAt: time.Now().UTC(),
	})
	return nil
}

// PublishRaw publishes an already-built, already-validated command envelope
// to an arbitrary topic, skipping the config.Device lookup PublishCommand
// requires. It exists for v2 devices, which this sandboxed process has no
// knowledge of at all - their manifest lives in the root process's registry
// (see internal/apigateway/device_v2.go, which resolves the device, finds
// the declared command and validates its parameters before ever building
// the envelope passed here). It is reached only via internal/api's
// loopback-only endpoint (ADR-009), never from the public edge directly.
func (g *Gateway) PublishRaw(ctx context.Context, topic string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(topic) == "" {
		return errors.New("topic is required")
	}
	if _, err := validateCommand(payload); err != nil {
		return fmt.Errorf("invalid command payload: %w", err)
	}
	if err := g.client.Publish(ctx, topic, append([]byte(nil), payload...), qosAtLeastOnce, false); err != nil {
		return fmt.Errorf("publish command to %q: %w", topic, err)
	}
	return nil
}

// recordAutomationCommand is best-effort: a persistence failure never
// unwinds an already-published command, it only surfaces through the
// optional AutomationLogger extension (same posture as outbox failures,
// see OutboxLogger's call sites in handleMessage).
func (g *Gateway) recordAutomationCommand(ctx context.Context, command registry.AutomationCommand) {
	if g.automationRecorder == nil {
		return
	}
	if err := g.automationRecorder.RecordAutomationCommand(ctx, command); err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "command", err)
		}
	}
}

func (g *Gateway) handleMessage(ctx context.Context, topic string, payload []byte, retained bool) {
	g.configMu.RLock()
	route, exists := g.routes[topic]
	if !exists {
		g.configMu.RUnlock()
		return
	}
	toOutbox, forward := g.toOutbox[topic]
	forwards := append([]config.Route(nil), g.forwards[topic]...)
	g.configMu.RUnlock()
	message, err := validateInbound(route, topic, payload, retained)
	if err != nil {
		g.rejectedMessages.Add(1)
		g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: err.Error()})
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Outcome: "rejected", Detail: err.Error()})
		return
	}
	g.acceptedMessages.Add(1)
	g.logger.Accepted(ctx, message)
	g.recordEvent(ActivityEvent{Timestamp: message.Timestamp, DeviceID: message.DeviceID, Kind: message.Kind, Topic: message.Topic, Outcome: "accepted"})
	switch message.Kind {
	case Event:
		g.recordAutomationEvent(ctx, message)
		g.fireAutomationRules(ctx, message)
	case CommandResult:
		g.recordAutomationCommandResult(ctx, message)
	}
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
		commandPayload, commandID, err := transformJSONCommand(forward.Transform.CommandType, message.Payload)
		if err == nil {
			err = g.client.Publish(ctx, forward.DestinationTopic, commandPayload, forward.QoS, forward.Retain)
		}
		if err != nil {
			g.localRoutesFailed.Add(1)
			g.rejectedMessages.Add(1)
			g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: route.kind, Topic: topic, Reason: fmt.Sprintf("route %s failed: %v", forward.ID, err)})
			g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Topic: forward.DestinationTopic, Outcome: "route_failed", Detail: forward.ID + ": " + err.Error()})
		} else {
			g.localRoutesPublished.Add(1)
			g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Topic: forward.DestinationTopic, Outcome: "route_published", Detail: forward.ID})
			g.recordAutomationCommand(ctx, registry.AutomationCommand{
				CommandID:          commandID,
				DeviceID:           route.deviceID,
				Topic:              forward.DestinationTopic,
				RouteID:            forward.ID,
				CausationMessageID: message.MessageID,
				CausationKind:      string(message.Kind),
				CausationDeviceID:  message.DeviceID,
				PublishedAt:        time.Now().UTC(),
			})
		}
	}
}

// recordAutomationEvent persists an accepted event-kind message. Best-effort:
// see recordAutomationCommand's doc comment.
func (g *Gateway) recordAutomationEvent(ctx context.Context, message Message) {
	if g.automationRecorder == nil {
		return
	}
	err := g.automationRecorder.RecordAutomationEvent(ctx, registry.AutomationEvent{
		EventID:    message.MessageID,
		DeviceID:   message.DeviceID,
		Topic:      message.Topic,
		EventType:  eventPayloadType(message.Payload),
		Payload:    message.Payload,
		OccurredAt: message.Timestamp,
	})
	if err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "event", err)
		}
	}
}

// eventPayloadType best-effort extracts a top-level "type" string field from
// an event payload, matching devicemanifest.Event.Type's convention. It is
// purely for indexing/presentation - an empty result (absent, non-string,
// malformed JSON) is never an error, since validateInbound never required
// this field.
func eventPayloadType(payload []byte) string {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.Type
}

// fireAutomationRules is the Marco 5 item 4 execution engine: for an
// accepted event, match enabled automation rules and, for each match whose
// condition passes, publish its action synchronously - the same delivery
// model local routes already use (transformJSONCommand below), not the
// outbox (there is no VPS consumer for it yet, and outbox's
// priority/eviction model is tuned for bandwidth, not local urgency).
// Best-effort throughout: a rule-matching or execution failure never
// affects the accepted message itself.
func (g *Gateway) fireAutomationRules(ctx context.Context, message Message) {
	if g.automationRecorder == nil {
		return
	}
	eventType := eventPayloadType(message.Payload)
	if eventType == "" {
		return
	}
	rules, err := g.automationRecorder.MatchAutomationRules(ctx, message.DeviceID, eventType)
	if err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "rule_match", err)
		}
		return
	}
	for _, rule := range rules {
		g.fireAutomationRule(ctx, rule, message)
	}
}

// fireAutomationRule applies one rule's dedup, condition and firing-rate
// checks in order - the first one that doesn't pass skips this rule
// without affecting any other matched rule - then publishes its action.
// The returned (fired, reason) is purely informational: fireAutomationRules'
// real-event loop ignores it, but TestAutomationRule (below) surfaces it
// directly to the operator who asked for a manual test.
func (g *Gateway) fireAutomationRule(ctx context.Context, rule registry.AutomationRule, message Message) (fired bool, reason string) {
	exists, err := g.automationRecorder.AutomationCommandExistsForRuleAndCausation(ctx, rule.ID, message.MessageID)
	if err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "rule_dedup", err)
		}
		return false, fmt.Sprintf("dedup check failed: %v", err)
	}
	if exists {
		return false, "duplicate"
	}
	if rule.ConditionJSON != "" {
		matched, err := automationrule.Evaluate(json.RawMessage(rule.ConditionJSON), message.Payload)
		if err != nil {
			if logger, ok := g.logger.(AutomationLogger); ok {
				logger.AutomationRecordFailed(ctx, "rule_condition", err)
			}
			return false, fmt.Sprintf("condition error: %v", err)
		}
		if !matched {
			return false, "condition not met"
		}
	}
	count, err := g.automationRecorder.CountAutomationCommandsForRuleSince(ctx, rule.ID, time.Now().UTC().Add(-ruleFiringWindow))
	if err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "rule_rate_limit", err)
		}
		return false, fmt.Sprintf("rate limit check failed: %v", err)
	}
	if count >= maxRuleFiringsPerRule {
		return false, "rate limited"
	}

	g.configMu.RLock()
	device, ok := g.devices[rule.ActionDeviceID]
	g.configMu.RUnlock()
	if !ok || device.Enabled == nil || !*device.Enabled || device.Topics.Command == "" {
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: rule.SourceDeviceID, Outcome: "rule_failed", Detail: rule.ID + ": action device is not enabled with a command topic"})
		return false, "action device unavailable"
	}
	payload, commandID, err := transformJSONCommand(rule.ActionCommandType, []byte(rule.ActionParametersJSON))
	if err != nil {
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: rule.SourceDeviceID, Topic: device.Topics.Command, Outcome: "rule_failed", Detail: rule.ID + ": " + err.Error()})
		return false, fmt.Sprintf("invalid action parameters: %v", err)
	}
	if err := g.client.Publish(ctx, device.Topics.Command, payload, qosAtLeastOnce, false); err != nil {
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: rule.SourceDeviceID, Topic: device.Topics.Command, Outcome: "rule_failed", Detail: rule.ID + ": " + err.Error()})
		return false, fmt.Sprintf("publish failed: %v", err)
	}
	g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: rule.SourceDeviceID, Topic: device.Topics.Command, Outcome: "rule_fired", Detail: rule.ID})
	g.recordAutomationCommand(ctx, registry.AutomationCommand{
		CommandID:          commandID,
		DeviceID:           rule.ActionDeviceID,
		Topic:              device.Topics.Command,
		RuleID:             rule.ID,
		CausationMessageID: message.MessageID,
		CausationKind:      string(Event),
		CausationDeviceID:  message.DeviceID,
		PublishedAt:        time.Now().UTC(),
	})
	return true, "fired"
}

// TestAutomationRule synthesizes an event-kind message for rule's source
// device from an operator-supplied payload and runs it through the exact
// fireAutomationRule path a real MQTT event would: dedup, condition
// evaluation, rate limit, and, if the condition passes, actually publishing
// the action command to the real device. It never records an
// AutomationEvent row - there is no real accepted message behind this, so
// nothing should claim there was one - but a resulting fired command is
// recorded exactly like a real one, including counting toward the shared
// rate limit.
//
// A disabled rule is refused outright (reason "rule is disabled"): the real
// pipeline (MatchAutomationRules) never fires a disabled rule, and silently
// testing past that would let "testar" move real hardware for a rule the
// operator explicitly turned off.
func (g *Gateway) TestAutomationRule(ctx context.Context, ruleID string, payload []byte) (fired bool, reason string, err error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	if g.automationRecorder == nil {
		return false, "", errors.New("automation is not configured")
	}
	rule, err := g.automationRecorder.GetAutomationRule(ctx, ruleID)
	if err != nil {
		return false, "", err
	}
	if !rule.Enabled {
		return false, "rule is disabled", nil
	}
	messageID, err := newRandomID()
	if err != nil {
		return false, "", err
	}
	message := Message{
		DeviceID:  rule.SourceDeviceID,
		Kind:      Event,
		MessageID: messageID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	fired, reason = g.fireAutomationRule(ctx, rule, message)
	return fired, reason, nil
}

// recordAutomationCommandResult best-effort correlates an inbound
// command-result message to the command_id it responds to, when the
// payload carries one - optional, since no firmware embeds it yet (see
// docs/api-v1.md). A missing or malformed command_id is not an error: the
// message was already accepted and is simply left uncorrelated.
func (g *Gateway) recordAutomationCommandResult(ctx context.Context, message Message) {
	if g.automationRecorder == nil {
		return
	}
	var envelope struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(message.Payload, &envelope); err != nil || !uuidPattern.MatchString(envelope.CommandID) {
		return
	}
	_, err := g.automationRecorder.RecordAutomationCommandResult(ctx, registry.AutomationCommandResult{
		CommandID:       envelope.CommandID,
		ResultMessageID: message.MessageID,
		Payload:         message.Payload,
		RecordedAt:      time.Now().UTC(),
	})
	if err != nil {
		if logger, ok := g.logger.(AutomationLogger); ok {
			logger.AutomationRecordFailed(ctx, "command_result", err)
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

// newRandomID generates a random UUID v4 without pulling in an external
// dependency - internal/mqtt otherwise has none.
func newRandomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func transformJSONCommand(commandType string, parameters []byte) (payload []byte, commandID string, err error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(parameters, &object); err != nil || object == nil {
		return nil, "", errors.New("route payload must be a JSON object")
	}
	commandID, err = newRandomID()
	if err != nil {
		return nil, "", err
	}
	payload, err = json.Marshal(struct {
		CommandID  string          `json:"command_id"`
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}{commandID, commandType, parameters})
	if err != nil {
		return nil, "", err
	}
	return payload, commandID, nil
}

func validateInbound(route route, topic string, payload []byte, retained bool) (Message, error) {
	if route.kind == Event && retained {
		return Message{}, errors.New("event messages must not be retained")
	}
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
		Retained:  retained,
	}, nil
}

func validateCommand(payload []byte) (commandID string, err error) {
	if !utf8.Valid(payload) {
		return "", errors.New("payload is not valid UTF-8")
	}
	var command struct {
		CommandID  string          `json:"command_id"`
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	var parameters map[string]json.RawMessage
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return "", errors.New("payload must be a JSON object")
	}
	if err := json.Unmarshal(payload, &command); err != nil {
		return "", fmt.Errorf("decode command: %w", err)
	}
	if !uuidPattern.MatchString(command.CommandID) {
		return "", errors.New("command_id must be a UUID")
	}
	if strings.TrimSpace(command.Type) == "" {
		return "", errors.New("type is required")
	}
	if len(command.Parameters) == 0 || json.Unmarshal(command.Parameters, &parameters) != nil || parameters == nil {
		return "", errors.New("parameters must be a JSON object")
	}
	return command.CommandID, nil
}
