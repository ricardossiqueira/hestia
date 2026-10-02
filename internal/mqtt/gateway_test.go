package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
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
	gateway, err := New(testConfig(), client, logger, nil, nil)
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

func TestGatewaySnapshotCountsPayloadFreeOutcomes(t *testing.T) {
	cfg := testConfig()
	cfg.Devices[0].Forwarding.TelemetryToVPS = true
	cfg.Routes = []config.Route{{
		ID:               "to-command",
		SourceTopic:      "devices/esp32-sala/telemetry",
		DestinationTopic: "devices/esp32-sala/command",
		Transform:        config.RouteTransform{Type: "json_command", CommandType: "render"},
		QoS:              1,
	}}
	client := &fakeClient{}
	queue := &fakeOutbox{result: outbox.EnqueueResult{Stored: true}}
	gateway, err := New(cfg, client, &recordingLogger{}, queue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))
	client.deliver("devices/esp32-sala/event", []byte(`bad`))

	snapshot := gateway.Snapshot()
	if !snapshot.Started || snapshot.StartedAt == nil || !snapshot.MQTTConnected || snapshot.Subscriptions != 4 {
		t.Errorf("gateway state = %#v", snapshot)
	}
	if snapshot.AcceptedMessages != 1 || snapshot.RejectedMessages != 1 || snapshot.LocalRoutesPublished != 1 || snapshot.LocalRoutesFailed != 0 || snapshot.OutboxStored != 1 {
		t.Errorf("gateway counters = %#v", snapshot)
	}
}

func TestGatewaySnapshotCountsRouteAndOutboxFailures(t *testing.T) {
	cfg := testConfig()
	cfg.Devices[0].Forwarding.TelemetryToVPS = true
	cfg.Routes = []config.Route{{
		ID: "to-command", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/esp32-sala/command",
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render"}, QoS: 1,
	}}
	client := &fakeClient{publishErr: errors.New("unavailable")}
	queue := &fakeOutbox{err: errors.New("disk full")}
	gateway, err := New(cfg, client, &recordingLogger{}, queue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))
	snapshot := gateway.Snapshot()
	if snapshot.AcceptedMessages != 1 || snapshot.RejectedMessages != 1 || snapshot.LocalRoutesFailed != 1 || snapshot.OutboxFailed != 1 {
		t.Errorf("gateway counters = %#v", snapshot)
	}
}

func TestGatewayRejectsMalformedInboundMessage(t *testing.T) {
	client := &fakeClient{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, nil)
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

func TestGatewayRejectsRetainedEvent(t *testing.T) {
	client := &fakeClient{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	client.deliverRetained("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))

	if len(logger.accepted) != 0 {
		t.Errorf("accepted messages = %d, want 0", len(logger.accepted))
	}
	if len(logger.rejected) != 1 {
		t.Fatalf("rejected messages = %d, want 1", len(logger.rejected))
	}
	if got := logger.rejected[0]; got.Kind != Event || !strings.Contains(got.Reason, "retained") {
		t.Errorf("rejected message = %#v, want Kind=%q and Reason mentioning \"retained\"", got, Event)
	}
	if snapshot := gateway.Snapshot(); snapshot.RejectedMessages != 1 {
		t.Errorf("snapshot.RejectedMessages = %d, want 1", snapshot.RejectedMessages)
	}
}

func TestGatewayDoesNotSubscribeDisabledDevices(t *testing.T) {
	cfg := testConfig()
	disabled := false
	cfg.Devices[0].Enabled = &disabled
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{}, nil, nil)
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
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
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
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
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
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
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
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
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
	disabledGateway, err := New(disabledConfig, &fakeClient{}, &recordingLogger{}, nil, nil)
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
	gateway, err := New(cfg, client, &recordingLogger{}, nil, nil)
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

func TestGatewayRecordsAcceptedEventAsAutomationEvent(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed"}`)
	client.deliver("devices/esp32-sala/event", payload)

	if len(recorder.events) != 1 {
		t.Fatalf("recorded events = %#v, want 1", recorder.events)
	}
	got := recorder.events[0]
	if got.EventID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" || got.DeviceID != "esp32-sala" ||
		got.Topic != "devices/esp32-sala/event" || got.EventType != "button_pressed" || string(got.Payload) != string(payload) {
		t.Errorf("recorded event = %#v", got)
	}
	if len(recorder.commands) != 0 || len(recorder.results) != 0 {
		t.Errorf("unexpected command/result recordings = %#v / %#v", recorder.commands, recorder.results)
	}
}

func TestGatewayRecordsRouteCommandWithCausation(t *testing.T) {
	cfg := testConfig()
	cfg.Devices = append(cfg.Devices, config.Device{ID: "display", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/display/command"}})
	cfg.Routes = []config.Route{{
		ID: "status-to-display", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/display/command", QoS: 1,
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_status"},
	}}
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{}
	gateway, err := New(cfg, client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","cpu_pct":24.6}`))

	if len(recorder.commands) != 1 {
		t.Fatalf("recorded commands = %#v, want 1", recorder.commands)
	}
	got := recorder.commands[0]
	if got.CommandID == "" || got.DeviceID != "esp32-sala" || got.Topic != "devices/display/command" ||
		got.RouteID != "status-to-display" || got.CausationMessageID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" ||
		got.CausationKind != string(Telemetry) || got.CausationDeviceID != "esp32-sala" {
		t.Errorf("recorded command = %#v", got)
	}
	if got.CommandID != mustCommandID(t, client.published[0].payload) {
		t.Errorf("recorded command_id %q does not match published payload", got.CommandID)
	}
}

func TestGatewayPublishCommandRecordsWithoutCausation(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := gateway.PublishCommand(context.Background(), "esp32-sala", validCommand()); err != nil {
		t.Fatal(err)
	}

	if len(recorder.commands) != 1 {
		t.Fatalf("recorded commands = %#v, want 1", recorder.commands)
	}
	got := recorder.commands[0]
	if got.CommandID != "a9f2290d-d1ee-4cbc-841d-03e29a7f028c" || got.DeviceID != "esp32-sala" ||
		got.Topic != "devices/esp32-sala/command" ||
		got.RouteID != "" || got.CausationMessageID != "" || got.CausationKind != "" || got.CausationDeviceID != "" {
		t.Errorf("recorded command = %#v, want no causation", got)
	}
}

func TestGatewayPublishRawSkipsDeviceLookup(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// "led-sala" is not in testConfig()'s devices at all - PublishRaw must
	// not care, unlike PublishCommand.
	if err := gateway.PublishRaw(context.Background(), "devices/led-sala/command", validCommand()); err != nil {
		t.Fatal(err)
	}
	if len(client.published) != 1 {
		t.Fatalf("published = %#v, want 1 message", client.published)
	}
	got := client.published[0]
	if got.topic != "devices/led-sala/command" || got.qos != qosAtLeastOnce || got.retain {
		t.Errorf("published = %#v", got)
	}
}

func TestGatewayPublishRawRejectsInvalidEnvelope(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := gateway.PublishRaw(context.Background(), "devices/led-sala/command", []byte(`{"not":"a command"}`)); err == nil {
		t.Fatal("PublishRaw() error = nil, want invalid envelope rejection")
	}
	if len(client.published) != 0 {
		t.Fatalf("published = %#v, want nothing", client.published)
	}
}

func TestGatewayCorrelatesCommandResultWhenCommandIDPresent(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{resultMatch: true}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/command-result", []byte(`{"message_id":"e9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:00Z","command_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","status":"ok"}`))

	if len(logger.accepted) != 1 {
		t.Fatalf("accepted messages = %d, want 1", len(logger.accepted))
	}
	if len(recorder.results) != 1 {
		t.Fatalf("recorded results = %#v, want 1", recorder.results)
	}
	got := recorder.results[0]
	if got.CommandID != "a9f2290d-d1ee-4cbc-841d-03e29a7f028c" || got.ResultMessageID != "e9f2290d-d1ee-4cbc-841d-03e29a7f028c" {
		t.Errorf("recorded result = %#v", got)
	}
}

func TestGatewayAcceptsCommandResultWithoutCommandID(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/command-result", []byte(`{"message_id":"e9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:00Z","status":"ok"}`))

	if len(logger.accepted) != 1 {
		t.Fatalf("accepted messages = %d, want 1", len(logger.accepted))
	}
	if len(recorder.results) != 0 {
		t.Errorf("recorded results = %#v, want none", recorder.results)
	}
}

func TestGatewayKeepsAcceptingWhenAutomationRecordingFails(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{eventErr: errors.New("disk full")}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))

	if len(logger.accepted) != 1 {
		t.Errorf("accepted messages = %d, want 1 (recording failure must not reject the message)", len(logger.accepted))
	}
	if len(logger.automationFailures) != 1 || logger.automationFailures[0] != "event" {
		t.Errorf("automation failures = %#v, want [\"event\"]", logger.automationFailures)
	}
}

func automationTestConfig() config.Config {
	cfg := testConfig()
	cfg.Devices = append(cfg.Devices, config.Device{
		ID: "led-2", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/led-2/command"},
	})
	return cfg
}

func automationTestRule() registry.AutomationRule {
	return registry.AutomationRule{
		ID: "led1-to-led2", Enabled: true,
		SourceDeviceID: "esp32-sala", EventType: "button_pressed",
		ActionDeviceID: "led-2", ActionCommandType: "set_led", ActionParametersJSON: `{"on":true}`,
	}
}

func TestFireAutomationRulesPublishesActionWhenConditionPasses(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.ConditionJSON = `{"==": [{"var": "pressed"}, true]}`
	recorder := &fakeAutomationRecorder{rules: map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {rule}}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed","pressed":true}`))

	if len(client.published) != 1 {
		t.Fatalf("published = %#v, want 1", client.published)
	}
	publication := client.published[0]
	if publication.topic != "devices/led-2/command" || publication.qos != 1 || publication.retain {
		t.Errorf("publication = %#v", publication)
	}
	commandID := mustCommandID(t, publication.payload)
	if len(recorder.commands) != 1 {
		t.Fatalf("recorded commands = %#v, want 1", recorder.commands)
	}
	got := recorder.commands[0]
	if got.CommandID != commandID || got.DeviceID != "led-2" || got.Topic != "devices/led-2/command" ||
		got.RuleID != "led1-to-led2" || got.CausationMessageID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" ||
		got.CausationKind != string(Event) || got.CausationDeviceID != "esp32-sala" {
		t.Errorf("recorded command = %#v", got)
	}
	events, _ := gateway.RecentEvents(EventFilter{})
	found := false
	for _, event := range events {
		if event.Outcome == "rule_fired" && event.Detail == "led1-to-led2" {
			found = true
		}
	}
	if !found {
		t.Errorf("RecentEvents() = %#v, want a rule_fired entry", events)
	}
}

func TestFireAutomationRulesSkipsWhenConditionFails(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.ConditionJSON = `{"==": [{"var": "pressed"}, false]}`
	recorder := &fakeAutomationRecorder{rules: map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {rule}}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed","pressed":true}`))

	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none (condition false)", client.published)
	}
	if len(recorder.commands) != 0 {
		t.Errorf("recorded commands = %#v, want none", recorder.commands)
	}
}

func TestFireAutomationRulesSkipsOnDedup(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{
		rules:      map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {automationTestRule()}},
		dedupExist: true,
	}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed"}`))

	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none (already fired for this event)", client.published)
	}
}

func TestFireAutomationRulesSkipsWhenRateLimited(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{
		rules:      map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {automationTestRule()}},
		firedCount: maxRuleFiringsPerRule,
	}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed"}`))

	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none (rate limited)", client.published)
	}
}

func TestFireAutomationRulesRecordsFailureWhenActionDeviceMissing(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.ActionDeviceID = "no-such-device"
	recorder := &fakeAutomationRecorder{rules: map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {rule}}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed"}`))

	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none", client.published)
	}
	events, _ := gateway.RecentEvents(EventFilter{})
	found := false
	for _, event := range events {
		if event.Outcome == "rule_failed" && event.Detail != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("RecentEvents() = %#v, want a rule_failed entry", events)
	}
}

func TestFireAutomationRulesRecordsFailureWhenPublishFails(t *testing.T) {
	client := &fakeClient{publishErr: errors.New("network error")}
	recorder := &fakeAutomationRecorder{rules: map[string][]registry.AutomationRule{"esp32-sala|button_pressed": {automationTestRule()}}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","type":"button_pressed"}`))

	if len(recorder.commands) != 0 {
		t.Errorf("recorded commands = %#v, want none (publish failed)", recorder.commands)
	}
	events, _ := gateway.RecentEvents(EventFilter{})
	found := false
	for _, event := range events {
		if event.Outcome == "rule_failed" && event.Detail == "led1-to-led2: network error" {
			found = true
		}
	}
	if !found {
		t.Errorf("RecentEvents() = %#v, want a rule_failed entry mentioning the publish error", events)
	}
}

func TestTestAutomationRuleFiresWhenConditionPasses(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.ConditionJSON = `{"==": [{"var": "pressed"}, true]}`
	recorder := &fakeAutomationRecorder{byID: map[string]registry.AutomationRule{rule.ID: rule}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	fired, reason, err := gateway.TestAutomationRule(context.Background(), rule.ID, []byte(`{"pressed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !fired || reason != "fired" {
		t.Errorf("TestAutomationRule() = %v, %q, want true, \"fired\"", fired, reason)
	}
	if len(client.published) != 1 || client.published[0].topic != "devices/led-2/command" {
		t.Errorf("published = %#v, want one publish to devices/led-2/command", client.published)
	}
	if len(recorder.events) != 0 {
		t.Errorf("recorded automation events = %#v, want none (no real message was accepted)", recorder.events)
	}
}

func TestTestAutomationRuleReportsConditionNotMet(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.ConditionJSON = `{"==": [{"var": "pressed"}, false]}`
	recorder := &fakeAutomationRecorder{byID: map[string]registry.AutomationRule{rule.ID: rule}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	fired, reason, err := gateway.TestAutomationRule(context.Background(), rule.ID, []byte(`{"pressed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if fired || reason != "condition not met" {
		t.Errorf("TestAutomationRule() = %v, %q, want false, \"condition not met\"", fired, reason)
	}
	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none", client.published)
	}
}

func TestTestAutomationRuleRefusesDisabledRule(t *testing.T) {
	client := &fakeClient{}
	rule := automationTestRule()
	rule.Enabled = false
	recorder := &fakeAutomationRecorder{byID: map[string]registry.AutomationRule{rule.ID: rule}}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	fired, reason, err := gateway.TestAutomationRule(context.Background(), rule.ID, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if fired || reason != "rule is disabled" {
		t.Errorf("TestAutomationRule() = %v, %q, want false, \"rule is disabled\"", fired, reason)
	}
	if len(client.published) != 0 {
		t.Errorf("published = %#v, want none (rule is disabled)", client.published)
	}
}

func TestTestAutomationRuleReturnsNotFoundForUnknownRule(t *testing.T) {
	client := &fakeClient{}
	recorder := &fakeAutomationRecorder{}
	gateway, err := New(automationTestConfig(), client, &recordingLogger{}, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, err = gateway.TestAutomationRule(context.Background(), "no-such-rule", []byte(`{}`))
	if !errors.Is(err, registry.ErrAutomationRuleNotFound) {
		t.Errorf("TestAutomationRule() error = %v, want ErrAutomationRuleNotFound", err)
	}
}

func mustCommandID(t *testing.T, payload []byte) string {
	t.Helper()
	var command struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(payload, &command); err != nil {
		t.Fatal(err)
	}
	return command.CommandID
}

func TestGatewayEnqueuesOnlyConfiguredVPSForwardingBeforeLocalRoutes(t *testing.T) {
	cfg := testConfig()
	cfg.Devices[0].Forwarding.TelemetryToVPS = true
	cfg.Devices = append(cfg.Devices, config.Device{ID: "display", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/display/command"}})
	cfg.Routes = []config.Route{{
		ID: "status-to-display", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/display/command", QoS: 1,
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_status"},
	}}
	queue := &fakeOutbox{}
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{}, queue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","cpu_pct":24.6}`))
	if len(queue.messages) != 1 {
		t.Fatalf("outbox messages = %#v", queue.messages)
	}
	if message := queue.messages[0]; message.Kind != outbox.Telemetry || message.Topic != "devices/esp32-sala/telemetry" {
		t.Errorf("outbox message = %#v", message)
	}
	if len(client.published) != 1 {
		t.Errorf("local route publications = %#v, want one", client.published)
	}

	client.deliver("devices/esp32-sala/state", []byte(`{"message_id":"e9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:00Z"}`))
	if len(queue.messages) != 1 {
		t.Errorf("state was unexpectedly enqueued: %#v", queue.messages)
	}
}

func TestGatewayKeepsLocalRoutesWhenOutboxFails(t *testing.T) {
	cfg := testConfig()
	cfg.Devices[0].Forwarding.TelemetryToVPS = true
	cfg.Devices = append(cfg.Devices, config.Device{ID: "display", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/display/command"}})
	cfg.Routes = []config.Route{{
		ID: "status-to-display", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/display/command", QoS: 1,
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_status"},
	}}
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{}, &fakeOutbox{err: errors.New("disk full")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))
	if len(client.published) != 1 {
		t.Errorf("local route did not continue after outbox failure: %#v", client.published)
	}
}

func TestGatewayRequiresOutboxWhenVPSForwardingIsEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Devices[0].Forwarding.EventsToVPS = true
	if _, err := New(cfg, &fakeClient{}, &recordingLogger{}, nil, nil); err == nil || !strings.Contains(err.Error(), "outbox is required") {
		t.Fatalf("New(, nil) error = %v", err)
	}
}

func TestGatewayApplyChangesPolicyWithoutRestart(t *testing.T) {
	client := &fakeClient{}
	logger := &recordingLogger{}
	gateway, err := New(testConfig(), client, logger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	enabled := true
	updated := config.Config{Devices: []config.Device{{
		ID: "led-2", Type: "esp32", Enabled: &enabled,
		Topics: config.Topics{State: "devices/led-2/state", Command: "devices/led-2/command"},
	}}}
	if err := gateway.Apply(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if gateway.Snapshot().Subscriptions != 1 {
		t.Fatalf("subscriptions = %d", gateway.Snapshot().Subscriptions)
	}
	if _, ok := client.subscriptions["devices/led-2/state"]; !ok {
		t.Fatal("new topic was not subscribed")
	}
	if err := gateway.PublishCommand(context.Background(), "esp32-sala", validCommand()); err == nil {
		t.Fatal("removed device accepted command")
	}
	if err := gateway.PublishCommand(context.Background(), "led-2", validCommand()); err != nil {
		t.Fatalf("new device command: %v", err)
	}

	// A stale transport subscription is harmless even for a minimal Client
	// without Unsubscribe: the swapped policy rejects it before validation.
	client.deliver("devices/esp32-sala/state", []byte(`{"message_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:00Z"}`))
	if len(logger.accepted) != 0 {
		t.Fatalf("stale topic was accepted: %#v", logger.accepted)
	}
}

func TestGatewayLastTelemetryReturnsMostRecentAcceptedMessage(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := gateway.LastTelemetry("esp32-sala"); ok {
		t.Fatal("LastTelemetry() ok = true before any message was received")
	}

	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","temperature_c":24.6}`))
	payload, observedAt, ok := gateway.LastTelemetry("esp32-sala")
	if !ok {
		t.Fatal("LastTelemetry() ok = false after an accepted telemetry message")
	}
	if !strings.Contains(string(payload), `"temperature_c":24.6`) {
		t.Errorf("LastTelemetry() payload = %s", payload)
	}
	if got := observedAt.Format(time.RFC3339); got != "2026-09-18T15:00:00Z" {
		t.Errorf("LastTelemetry() observedAt = %v", got)
	}

	// A later message replaces the cached one instead of accumulating.
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:05Z","temperature_c":25.1}`))
	payload, _, _ = gateway.LastTelemetry("esp32-sala")
	if !strings.Contains(string(payload), `"temperature_c":25.1`) {
		t.Errorf("LastTelemetry() did not update to the latest message: %s", payload)
	}

	// Non-telemetry kinds are not cached here.
	client.deliver("devices/esp32-sala/state", []byte(`{"message_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:10Z"}`))
	if _, _, ok := gateway.LastTelemetry("unknown-device"); ok {
		t.Fatal("LastTelemetry() ok = true for a device that never published telemetry")
	}
}

func TestGatewayRecentEventsCapturesAcceptedRejectedAndRouteOutcomes(t *testing.T) {
	cfg := testConfig()
	cfg.Devices = append(cfg.Devices, config.Device{ID: "display", Enabled: boolPtr(true), Topics: config.Topics{Command: "devices/display/command"}})
	cfg.Routes = []config.Route{{
		ID: "status-to-display", SourceTopic: "devices/esp32-sala/telemetry", DestinationTopic: "devices/display/command", QoS: 1,
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_status"},
	}}
	client := &fakeClient{}
	gateway, err := New(cfg, client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, hasMore := gateway.RecentEvents(EventFilter{}); len(got) != 0 || hasMore {
		t.Fatalf("RecentEvents() before any message = %#v, %v, want empty/false", got, hasMore)
	}

	// accepted + route_published (the route above forwards this telemetry
	// to devices/display/command).
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z","cpu_pct":24.6}`))
	// rejected (malformed JSON).
	client.deliver("devices/esp32-sala/state", []byte(`not json`))

	events, hasMore := gateway.RecentEvents(EventFilter{})
	if len(events) != 3 || hasMore {
		t.Fatalf("RecentEvents() = %#v, hasMore=%v, want 3/false", events, hasMore)
	}
	// Most recent first.
	if events[0].Outcome != "rejected" || events[0].DeviceID != "esp32-sala" {
		t.Errorf("events[0] = %#v", events[0])
	}
	if events[1].Outcome != "route_published" || events[1].Detail != "status-to-display" {
		t.Errorf("events[1] = %#v", events[1])
	}
	if events[2].Outcome != "accepted" || events[2].Kind != Telemetry || events[2].DeviceID != "esp32-sala" {
		t.Errorf("events[2] = %#v", events[2])
	}
	for _, event := range events {
		if event.Timestamp.IsZero() {
			t.Errorf("event %#v has a zero Timestamp", event)
		}
	}
	// Sequence is strictly increasing, oldest to newest - so descending
	// here, most-recent-first.
	if !(events[0].Sequence > events[1].Sequence && events[1].Sequence > events[2].Sequence) {
		t.Errorf("Sequence is not strictly decreasing across events: %d, %d, %d", events[0].Sequence, events[1].Sequence, events[2].Sequence)
	}
}

func TestGatewayRecentEventsFiltersByDeviceAndSince(t *testing.T) {
	client := &fakeClient{}
	cfg := testConfig()
	cfg.Devices = append(cfg.Devices, config.Device{ID: "led-2", Enabled: boolPtr(true), Topics: config.Topics{State: "devices/led-2/state"}})
	gateway, err := New(cfg, client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// "accepted" events use the payload's own declared timestamp
	// (message.Timestamp), not wall-clock time - see handleMessage.
	client.deliver("devices/esp32-sala/state", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))
	between := time.Date(2026, 9, 18, 15, 0, 0, 500_000_000, time.UTC)
	client.deliver("devices/led-2/state", []byte(`{"message_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","timestamp":"2026-09-18T15:00:01Z"}`))

	byDevice, _ := gateway.RecentEvents(EventFilter{DeviceID: "led-2"})
	if len(byDevice) != 1 || byDevice[0].DeviceID != "led-2" {
		t.Fatalf("RecentEvents(DeviceID) = %#v", byDevice)
	}

	bySince, _ := gateway.RecentEvents(EventFilter{Since: between})
	if len(bySince) != 1 || bySince[0].DeviceID != "led-2" {
		t.Fatalf("RecentEvents(Since) = %#v", bySince)
	}
}

func TestGatewayRecentEventsPaginatesWithBeforeSequenceAndHasMore(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		client.deliver("devices/esp32-sala/state", []byte(`not json`))
	}

	page1, hasMore := gateway.RecentEvents(EventFilter{Limit: 2})
	if len(page1) != 2 || !hasMore {
		t.Fatalf("page1 = %#v, hasMore=%v, want 2/true", page1, hasMore)
	}
	page2, hasMore := gateway.RecentEvents(EventFilter{Limit: 2, BeforeSequence: page1[len(page1)-1].Sequence})
	if len(page2) != 2 || !hasMore {
		t.Fatalf("page2 = %#v, hasMore=%v, want 2/true", page2, hasMore)
	}
	if page2[0].Sequence >= page1[len(page1)-1].Sequence {
		t.Errorf("page2 overlaps page1: page1 last = %d, page2 first = %d", page1[len(page1)-1].Sequence, page2[0].Sequence)
	}
	page3, hasMore := gateway.RecentEvents(EventFilter{Limit: 2, BeforeSequence: page2[len(page2)-1].Sequence})
	if len(page3) != 1 || hasMore {
		t.Fatalf("page3 = %#v, hasMore=%v, want 1/false (5 events, 2+2+1)", page3, hasMore)
	}
}

func TestGatewayRecentEventsBufferIsBoundedByMax(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRecentEvents+20; i++ {
		client.deliver("devices/esp32-sala/state", []byte(`not json`))
	}
	events, hasMore := gateway.RecentEvents(EventFilter{Limit: maxRecentEvents})
	if len(events) != maxRecentEvents || hasMore {
		t.Fatalf("RecentEvents(Limit: max) = %d events, hasMore=%v, want %d/false", len(events), hasMore, maxRecentEvents)
	}
}

func TestGatewayApplyPrunesLastTelemetryForRemovedDevice(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(testConfig(), client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/esp32-sala/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-09-18T15:00:00Z"}`))
	if _, _, ok := gateway.LastTelemetry("esp32-sala"); !ok {
		t.Fatal("setup: expected a cached telemetry reading")
	}

	if err := gateway.Apply(context.Background(), config.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := gateway.LastTelemetry("esp32-sala"); ok {
		t.Fatal("LastTelemetry() ok = true for a device removed by Apply")
	}
}

func validCommand() []byte {
	return []byte(`{"command_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","type":"set_output","parameters":{"pin":2,"value":true}}`)
}

func TestGatewayRejectsNilDependencies(t *testing.T) {
	if _, err := New(testConfig(), nil, &recordingLogger{}, nil, nil); err == nil {
		t.Error("New(, nil) error = nil for nil client")
	}
	if _, err := New(testConfig(), &fakeClient{}, nil, nil, nil); err == nil {
		t.Error("New(, nil) error = nil for nil logger")
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
func (c *fakeClient) Connected() bool               { return c.connected && !c.closed && c.connectErr == nil }
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
	c.subscriptions[topic](context.Background(), topic, payload, false)
}

func (c *fakeClient) deliverRetained(topic string, payload []byte) {
	c.subscriptions[topic](context.Background(), topic, payload, true)
}

type publication struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

type fakeOutbox struct {
	messages []outbox.Message
	err      error
	result   outbox.EnqueueResult
}

func (o *fakeOutbox) Enqueue(_ context.Context, message outbox.Message) (outbox.EnqueueResult, error) {
	o.messages = append(o.messages, message)
	if o.err != nil {
		return outbox.EnqueueResult{}, o.err
	}
	if o.result == (outbox.EnqueueResult{}) {
		return outbox.EnqueueResult{Stored: true}, nil
	}
	return o.result, nil
}

type fakeAutomationRecorder struct {
	events      []registry.AutomationEvent
	commands    []registry.AutomationCommand
	results     []registry.AutomationCommandResult
	eventErr    error
	commandErr  error
	resultMatch bool
	resultErr   error

	// rules is keyed by "deviceID|eventType" for MatchAutomationRules.
	rules      map[string][]registry.AutomationRule
	matchErr   error
	dedupExist bool
	dedupErr   error
	firedCount int
	countErr   error

	// byID is keyed by rule ID for GetAutomationRule.
	byID    map[string]registry.AutomationRule
	byIDErr error
}

func (r *fakeAutomationRecorder) RecordAutomationEvent(_ context.Context, event registry.AutomationEvent) error {
	r.events = append(r.events, event)
	return r.eventErr
}

func (r *fakeAutomationRecorder) RecordAutomationCommand(_ context.Context, command registry.AutomationCommand) error {
	r.commands = append(r.commands, command)
	return r.commandErr
}

func (r *fakeAutomationRecorder) RecordAutomationCommandResult(_ context.Context, result registry.AutomationCommandResult) (bool, error) {
	r.results = append(r.results, result)
	return r.resultMatch, r.resultErr
}

func (r *fakeAutomationRecorder) MatchAutomationRules(_ context.Context, deviceID, eventType string) ([]registry.AutomationRule, error) {
	if r.matchErr != nil {
		return nil, r.matchErr
	}
	return r.rules[deviceID+"|"+eventType], nil
}

func (r *fakeAutomationRecorder) AutomationCommandExistsForRuleAndCausation(_ context.Context, _, _ string) (bool, error) {
	return r.dedupExist, r.dedupErr
}

func (r *fakeAutomationRecorder) CountAutomationCommandsForRuleSince(_ context.Context, _ string, _ time.Time) (int, error) {
	return r.firedCount, r.countErr
}

func (r *fakeAutomationRecorder) GetAutomationRule(_ context.Context, id string) (registry.AutomationRule, error) {
	if r.byIDErr != nil {
		return registry.AutomationRule{}, r.byIDErr
	}
	rule, ok := r.byID[id]
	if !ok {
		return registry.AutomationRule{}, fmt.Errorf("%w: %q", registry.ErrAutomationRuleNotFound, id)
	}
	return rule, nil
}

type recordingLogger struct {
	accepted           []Message
	rejected           []RejectedMessage
	automationFailures []string
}

func (l *recordingLogger) AutomationRecordFailed(_ context.Context, kind string, _ error) {
	l.automationFailures = append(l.automationFailures, kind)
}

func (l *recordingLogger) Accepted(_ context.Context, message Message) {
	l.accepted = append(l.accepted, message)
}
func (l *recordingLogger) Rejected(_ context.Context, message RejectedMessage) {
	l.rejected = append(l.rejected, message)
}
