package mqtt

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
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

func TestGatewayClosesClientWhenStartingFails(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	led, err := store.AcceptV2Manifest(ctx, v2RuntimeLED, "test")
	if err != nil {
		t.Fatal(err)
	}
	registerV2RuntimeDevice(t, store, "led-source", "uid-source", led, "key-source")

	client := &fakeClient{subscribeErr: errors.New("broker unavailable")}
	gateway, err := New(client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	// At least one active v2 device is needed so Start() has a topic to
	// subscribe to, exercising the subscribe-failure path below.
	if err := gateway.EnableV2Runtime(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(ctx); err == nil {
		t.Fatal("Start() error = nil")
	}
	if !client.closed {
		t.Error("client was not closed after startup failure")
	}
}

func TestGatewayClosesClientWhenConnectingFails(t *testing.T) {
	client := &fakeClient{connectErr: errors.New("broker unavailable")}
	gateway, err := New(client, &recordingLogger{})
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
	gateway, err := New(client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}
	if client.connected {
		t.Error("client connected with canceled context")
	}
	if err := gateway.PublishRaw(ctx, "devices/esp32-sala/command", validCommand()); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishRaw() error = %v, want context canceled", err)
	}
	if len(client.published) != 0 {
		t.Error("command was published with canceled context")
	}
}

func TestGatewayPublishRawSkipsDeviceLookup(t *testing.T) {
	client := &fakeClient{}
	gateway, err := New(client, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	gateway, err := New(client, &recordingLogger{})
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

func TestGatewayRecentEventsBufferIsBoundedByMax(t *testing.T) {
	gateway, err := New(&fakeClient{}, &recordingLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRecentEvents+20; i++ {
		gateway.recordEvent(ActivityEvent{Outcome: "accepted"})
	}
	events, hasMore := gateway.RecentEvents(EventFilter{Limit: maxRecentEvents})
	if len(events) != maxRecentEvents || hasMore {
		t.Fatalf("RecentEvents(Limit: max) = %d events, hasMore=%v, want %d/false", len(events), hasMore, maxRecentEvents)
	}
}

func TestGatewayRejectsNilDependencies(t *testing.T) {
	if _, err := New(nil, &recordingLogger{}); err == nil {
		t.Error("New(nil, ) error = nil for nil client")
	}
	if _, err := New(&fakeClient{}, nil); err == nil {
		t.Error("New(, nil) error = nil for nil logger")
	}
}

func validCommand() []byte {
	return []byte(`{"command_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","type":"set_output","parameters":{"pin":2,"value":true}}`)
}

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
