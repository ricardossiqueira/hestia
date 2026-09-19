package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

func TestResolveCredentials(t *testing.T) {
	tests := []struct {
		name    string
		mqtt    config.MQTT
		env     map[string]string
		want    Credentials
		wantErr string
	}{
		{name: "no credentials configured", wantErr: "must be configured together"},
		{
			name: "credentials from environment",
			mqtt: config.MQTT{UsernameEnv: "MQTT_USER", PasswordEnv: "MQTT_PASSWORD"},
			env:  map[string]string{"MQTT_USER": "gateway", "MQTT_PASSWORD": "secret"},
			want: Credentials{Username: "gateway", Password: "secret"},
		},
		{
			name:    "only one environment variable name",
			mqtt:    config.MQTT{UsernameEnv: "MQTT_USER"},
			wantErr: "must be configured together",
		},
		{
			name:    "missing environment value",
			mqtt:    config.MQTT{UsernameEnv: "MQTT_USER", PasswordEnv: "MQTT_PASSWORD"},
			env:     map[string]string{"MQTT_USER": "gateway"},
			wantErr: "MQTT_PASSWORD is not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveCredentials(tt.mqtt, func(name string) string { return tt.env[name] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveCredentials() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ResolveCredentials() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGatewayStartsEnabledInboundTopicsAndLogsAcceptedMessage(t *testing.T) {
	client := &fakeClient{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !client.connected {
		t.Error("client was not connected")
	}
	wantTopics := map[string]bool{
		"devices/esp32-sala/telemetry":      true,
		"devices/esp32-sala/state":          true,
		"devices/esp32-sala/event":          true,
		"devices/esp32-sala/command-result": true,
	}
	if len(client.subscriptions) != len(wantTopics) {
		t.Fatalf("subscriptions = %d, want %d", len(client.subscriptions), len(wantTopics))
	}
	for topic := range client.subscriptions {
		if !wantTopics[topic] {
			t.Errorf("unexpected subscription %q", topic)
		}
	}

	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","temperature_c":24.6}`))
	if len(logger.accepted) != 1 {
		t.Fatalf("accepted messages = %d, want 1", len(logger.accepted))
	}
	got := logger.accepted[0]
	if got.DeviceID != "esp32-sala" || got.Kind != Telemetry || got.MessageID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" {
		t.Errorf("accepted message = %#v", got)
	}
}

func TestGatewayRejectsMalformedInboundMessage(t *testing.T) {
	client := &fakeClient{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		body string
	}{
		{"not json", "nope"},
		{"invalid UTF-8", string([]byte{0xff})},
		{"json array", `[]`},
		{"missing message id", `{"timestamp":"2026-09-18T15:00:00Z"}`},
		{"message id is not UUID", `{"message_id":"reading-1","timestamp":"2026-09-18T15:00:00Z"}`},
		{"missing timestamp", `{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad"}`},
		{"invalid timestamp", `{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"today"}`},
		{"timestamp is not UTC literal", `{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T12:00:00-03:00"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.deliver("devices/esp32-sala/event", []byte(tt.body))
		})
	}
	if len(logger.accepted) != 0 {
		t.Errorf("accepted messages = %d, want 0", len(logger.accepted))
	}
	if len(logger.rejected) != len(tests) {
		t.Errorf("rejected messages = %d, want %d", len(logger.rejected), len(tests))
	}
}

func TestGatewayDoesNotSubscribeDisabledDevices(t *testing.T) {
	cfg := testConfig()
	disabled := false
	cfg.Devices[0].Enabled = &disabled
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.subscriptions) != 0 {
		t.Errorf("subscriptions = %#v, want none", client.subscriptions)
	}
}

func TestGatewayClosesClientWhenStartingFails(t *testing.T) {
	client := &fakeClient{subscribeErr: errors.New("broker unavailable")}
	gateway, err := New(testConfig(), client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil")
	}
	if !client.closed {
		t.Error("client was not closed after startup failure")
	}
}

func TestGatewayClosesClientWhenConnectingFails(t *testing.T) {
	client := &fakeClient{connectErr: errors.New("broker unavailable")}
	gateway, err := New(testConfig(), client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil")
	}
	if !client.closed {
		t.Error("client was not closed after connection failure")
	}
}

func TestGatewayDoesNotUseClientWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}
	if client.connected {
		t.Error("client connected with canceled context")
	}
	if err := gateway.PublishCommand(ctx, "esp32-sala", validCommand()); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishCommand() error = %v, want context canceled", err)
	}
	if len(client.published) != 0 {
		t.Error("command was published with canceled context")
	}
}

func TestGatewayPublishesCommandOnlyForEnabledDeviceWithCommandTopic(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	payload := validCommand()
	if err := gateway.PublishCommand(context.Background(), "esp32-sala", payload); err != nil {
		t.Fatal(err)
	}
	if len(client.published) != 1 {
		t.Fatalf("published = %#v", client.published)
	}
	publication := client.published[0]
	if publication.topic != "devices/esp32-sala/command" || publication.qos != 1 || publication.retain {
		t.Errorf("publication = %#v", publication)
	}

	disabledConfig := testConfig()
	disabled := false
	disabledConfig.Devices[0].Enabled = &disabled
	disabledGateway, err := New(disabledConfig, &fakeClient{}, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := disabledGateway.PublishCommand(context.Background(), "esp32-sala", payload); err == nil {
		t.Error("PublishCommand() error = nil for disabled device")
	}
	if err := gateway.PublishCommand(context.Background(), "unknown", payload); err == nil {
		t.Error("PublishCommand() error = nil for unknown device")
	}
	if err := gateway.PublishCommand(context.Background(), "esp32-sala", []byte(`{"command_id":"not-a-uuid"}`)); err == nil {
		t.Error("PublishCommand() error = nil for malformed command")
	}
	client.publishErr = errors.New("network error")
	if err := gateway.PublishCommand(context.Background(), "esp32-sala", payload); err == nil {
		t.Error("PublishCommand() error = nil when client publish fails")
	}
}

func TestGatewayForwardsThroughGenericJSONCommandRoute(t *testing.T) {
	cfg := testConfig()
	cfg.Devices = append(cfg.Devices, config.Device{ID: "display", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/display/command"}})
	cfg.Routes = []config.Route{{
		ID: "status-to-display", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/display/command", QoS: 1,
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_status"},
	}}
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","cpu_pct":24.6}`)
	client.deliver("devices/esp32-sala/telemetry", source)
	if len(client.published) != 1 {
		t.Fatalf("published = %#v", client.published)
	}
	publication := client.published[0]
	if publication.topic != "devices/display/command" || publication.qos != 1 || publication.retain {
		t.Errorf("publication = %#v", publication)
	}
	var command struct {
		CommandID  string          `json:"command_id"`
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(publication.payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.CommandID == "" || command.Type != "render_status" || string(command.Parameters) != string(source) {
		t.Errorf("command = %#v", command)
	}
}

func validCommand() []byte {
	return []byte(`{"command_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","type":"set_output","parameters":{"pin":2,"value":true}}`)
}

func TestGatewayRejectsNilDependencies(t *testing.T) {
	if _, err := New(testConfig(), nil, &recordingLogger{}); err == nil {
		t.Error("New() error = nil for nil client")
	}
	if _, err := New(testConfig(), &fakeClient{}, nil); err == nil {
		t.Error("New() error = nil for nil logger")
	}
}

func testConfig() config.Config {
	enabled := true
	return config.Config{Devices: []config.Device{{
		ID:      "esp32-sala",
		Enabled: &enabled,
		Topics: config.Topics{
			Telemetry:     "devices/esp32-sala/telemetry",
			State:         "devices/esp32-sala/state",
			Event:         "devices/esp32-sala/event",
			Command:       "devices/esp32-sala/command",
			CommandResult: "devices/esp32-sala/command-result",
		},
	}}}
}

func boolPtr(value bool) *bool { return &value }

type fakeClient struct {
	connected     bool
	closed        bool
	connectErr    error
	subscribeErr  error
	publishErr    error
	subscriptions map[string]MessageHandler
	published     []publication
}

func (c *fakeClient) Connect(context.Context) error { c.connected = true; return c.connectErr }
func (c *fakeClient) Close()                        { c.closed = true }
func (c *fakeClient) Subscribe(_ context.Context, topic string, handler MessageHandler) error {
	if c.subscribeErr != nil {
		return c.subscribeErr
	}
	if c.subscriptions == nil {
		c.subscriptions = make(map[string]MessageHandler)
	}
	c.subscriptions[topic] = handler
	return nil
}
func (c *fakeClient) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	c.published = append(c.published, publication{topic: topic, payload: payload, qos: qos, retain: retain})
	return c.publishErr
}
func (c *fakeClient) deliver(topic string, payload []byte) {
	c.subscriptions[topic](context.Background(), topic, payload)
}

type publication struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

type recordingLogger struct {
	accepted []Message
	rejected []RejectedMessage
}

func (l *recordingLogger) Accepted(_ context.Context, message Message) {
	l.accepted = append(l.accepted, message)
}
func (l *recordingLogger) Rejected(_ context.Context, message RejectedMessage) {
	l.rejected = append(l.rejected, message)
}
