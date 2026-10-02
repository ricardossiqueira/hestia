// Package mqtt contains the gateway's MQTT-facing application logic.
package mqtt

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
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

// AutomationLogger is an optional operational logging extension for a v2
// automation rule's best-effort recording failures - these never affect
// message acceptance or command publication, so a Logger that doesn't
// implement this simply has nothing surface them.
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

// Gateway subscribes to active v2 device topics and publishes validated
// commands. It used to also route V1's statically-configured devices and
// local MQTT routes - retired along with the rest of V1 (see
// docs/decisions.md's V1-removal ADR). LocalRoutesPublished/Failed and the
// Outbox* counters below are kept on Snapshot (internal/diagnostics still
// reads them) but permanently read zero now: there is no local routing and
// no outbox/VPS-forwarding path for v2 devices yet - a known, documented
// gap, not an oversight.
type Gateway struct {
	client Client
	logger Logger

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

	// eventsMu guards recentEvents and nextEventSeq - a data-plane ring
	// buffer, separate from configMu (control-plane policy).
	eventsMu     sync.RWMutex
	recentEvents []ActivityEvent
	nextEventSeq uint64

	// telemetryMu guards lastTelemetry - see v2_telemetry.go.
	telemetryMu   sync.RWMutex
	lastTelemetry map[string]Message

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

// maxRecentEvents bounds the in-memory activity ring buffer (RecentEvents)
// the same way outbox/registry tables are bounded on disk - an edge device
// principle this codebase already applies everywhere state accumulates.
const maxRecentEvents = 200

// defaultEventsLimit is how many events RecentEvents returns per call when
// EventFilter.Limit is unset - a page for gateway-web's activity table, not
// the whole buffer every time.
const defaultEventsLimit = 50

// ruleFiringWindow and maxRuleFiringsPerRule bound how often a single v2
// automation rule may fire, standing in for true cycle detection: the
// protocol gives no way to prove an event was itself caused by an earlier
// command, so a per-rule firing-rate limit is what's actually enforceable
// today. Not configurable per rule yet - a fixed constant.
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
	// Outcome is one of "accepted", "rejected", "v2_rule_fired".
	Outcome string
	// Detail is the rejection reason, or the rule ID - never a payload.
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

// New constructs the MQTT gateway. Call EnableV2Runtime afterward to load
// the active v2 device policy before Start.
func New(client Client, logger Logger) (*Gateway, error) {
	if client == nil {
		return nil, errors.New("MQTT client is required")
	}
	if logger == nil {
		return nil, errors.New("MQTT logger is required")
	}
	return &Gateway{
		client:    client,
		logger:    logger,
		v2Routes:  make(map[string]v2Route),
		v2Devices: make(map[string]devicev2.Manifest),
	}, nil
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
		subscriptions = len(g.v2Topics)
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

// Start connects to the broker and subscribes to every active v2 device
// topic. A partial startup is closed so the caller can retry cleanly.
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

// PublishRaw publishes an already-built, already-validated command envelope
// to an arbitrary topic. It exists for v2 devices, which this sandboxed
// process has no knowledge of at all - their manifest lives in the root
// process's registry (see internal/apigateway/device_v2.go, which resolves
// the device, finds the declared command and validates its parameters
// before ever building the envelope passed here). It is reached only via
// internal/api's loopback-only endpoint (ADR-009), never from the public
// edge directly.
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

// eventPayloadType best-effort extracts a top-level "type" string field from
// an event payload. It is purely for indexing/presentation - an empty
// result (absent, non-string, malformed JSON) is never an error.
func eventPayloadType(payload []byte) string {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.Type
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
