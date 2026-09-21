package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
)

type publishCall struct {
	deviceID string
	payload  []byte
}

type fakePublisher struct {
	mu    sync.Mutex
	calls []publishCall
	err   error
}

func (f *fakePublisher) PublishCommand(ctx context.Context, deviceID string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, publishCall{deviceID: deviceID, payload: append([]byte(nil), payload...)})
	return nil
}

func (f *fakePublisher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePublisher) lastCall() publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type fakeStatus struct {
	snap mqtt.Snapshot
}

func (f fakeStatus) Snapshot() mqtt.Snapshot { return f.snap }

func testDevices() []config.Device {
	enabled := true
	return []config.Device{
		{
			ID:      "led-1",
			Type:    "esp32",
			Enabled: &enabled,
			Profile: "led.v1",
			Topics:  config.Topics{Command: "devices/led-1/command"},
		},
		{
			ID:      "opaque-1",
			Type:    "esp32",
			Enabled: &enabled,
			Topics:  config.Topics{Command: "devices/opaque-1/command"},
		},
	}
}

// newTestServer starts an httptest server directly on the Server's
// http.Handler (bypassing Start/net.Listen, which would bind a real port)
// so tests stay fast and hermetic. This is safe because server_test.go
// lives in package api and can reach the unexported s.http field. No auth
// is exercised here any more (ADR-013): this package is loopback-only and
// trusts its caller (internal/apigateway's reverse proxy) unconditionally.
func newTestServer(t *testing.T, publisher *fakePublisher, status fakeStatus) *httptest.Server {
	t.Helper()
	cfg := Config{
		Address:        "127.0.0.1:0",
		RequestTimeout: time.Second,
		Registry:       config.Config{Devices: testDevices()},
	}
	srv, err := New(cfg, publisher, status, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestListDevices(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	resp, err := client.ListDevices(context.Background(), connect.NewRequest(&apiv1.ListDevicesRequest{}))
	if err != nil {
		t.Fatalf("ListDevices() error = %v", err)
	}
	if len(resp.Msg.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(resp.Msg.Devices))
	}
	if resp.Msg.Devices[0].Id != "led-1" || resp.Msg.Devices[0].Profile != "led.v1" {
		t.Errorf("Devices[0] = %#v", resp.Msg.Devices[0])
	}
	if resp.Msg.Devices[1].Id != "opaque-1" || resp.Msg.Devices[1].Profile != "" {
		t.Errorf("Devices[1] = %#v", resp.Msg.Devices[1])
	}
}

func TestListDeviceCommands_WithProfile(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	req := connect.NewRequest(&apiv1.ListDeviceCommandsRequest{DeviceId: "led-1"})
	resp, err := client.ListDeviceCommands(context.Background(), req)
	if err != nil {
		t.Fatalf("ListDeviceCommands() error = %v", err)
	}
	if !resp.Msg.SchemaValidated {
		t.Fatal("SchemaValidated = false, want true")
	}
	if len(resp.Msg.Commands) != 1 || resp.Msg.Commands[0].Type != "set_led" {
		t.Fatalf("Commands = %#v", resp.Msg.Commands)
	}
	schema := resp.Msg.Commands[0].ParametersSchema
	if schema == nil || len(schema.Field) != 1 || schema.Field[0].GetName() != "on" || schema.Field[0].GetType() != descriptorpb.FieldDescriptorProto_TYPE_BOOL {
		t.Fatalf("ParametersSchema = %#v", schema)
	}
}

func TestListDeviceCommands_Opaque(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	req := connect.NewRequest(&apiv1.ListDeviceCommandsRequest{DeviceId: "opaque-1"})
	resp, err := client.ListDeviceCommands(context.Background(), req)
	if err != nil {
		t.Fatalf("ListDeviceCommands() error = %v", err)
	}
	if resp.Msg.SchemaValidated {
		t.Fatal("SchemaValidated = true, want false")
	}
	if len(resp.Msg.Commands) != 0 {
		t.Fatalf("Commands = %#v, want empty", resp.Msg.Commands)
	}
}

func TestPublishCommand_Success_SchemaValidated(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	params, _ := structpb.NewStruct(map[string]any{"on": true})
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "set_led", Parameters: params})
	resp, err := client.PublishCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("PublishCommand() error = %v", err)
	}
	if !resp.Msg.SchemaValidated {
		t.Error("SchemaValidated = false, want true")
	}
	if resp.Msg.CommandId == "" || resp.Msg.PublishedAt == nil {
		t.Errorf("response = %#v", resp.Msg)
	}
	if publisher.callCount() != 1 {
		t.Fatalf("publisher calls = %d, want 1", publisher.callCount())
	}
	call := publisher.lastCall()
	if call.deviceID != "led-1" {
		t.Errorf("published deviceID = %q", call.deviceID)
	}
	if !strings.Contains(string(call.payload), `"on":true`) {
		t.Errorf("payload = %s, want canonical on:true", call.payload)
	}
	if !strings.Contains(string(call.payload), resp.Msg.CommandId) {
		t.Errorf("payload command_id does not match response command_id: %s", call.payload)
	}
}

func TestPublishCommand_SchemaRejection_WrongType(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	params, _ := structpb.NewStruct(map[string]any{"on": "sim"})
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "set_led", Parameters: params})
	_, err := client.PublishCommand(context.Background(), req)
	if err == nil {
		t.Fatal("PublishCommand() error = nil, want schema rejection")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if publisher.callCount() != 0 {
		t.Fatalf("publisher calls = %d, want 0 (nothing should publish)", publisher.callCount())
	}
}

func TestPublishCommand_SchemaRejection_MissingField(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "set_led", Parameters: &structpb.Struct{}})
	_, err := client.PublishCommand(context.Background(), req)
	if err == nil {
		t.Fatal("PublishCommand() error = nil, want schema rejection")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if publisher.callCount() != 0 {
		t.Fatalf("publisher calls = %d, want 0", publisher.callCount())
	}
}

func TestPublishCommand_OpaqueFallback(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	params, _ := structpb.NewStruct(map[string]any{"ligado": true})
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "opaque-1", Type: "set_output", Parameters: params})
	resp, err := client.PublishCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("PublishCommand() error = %v", err)
	}
	if resp.Msg.SchemaValidated {
		t.Error("SchemaValidated = true, want false (no profile)")
	}
	if publisher.callCount() != 1 {
		t.Fatalf("publisher calls = %d, want 1", publisher.callCount())
	}
	if !strings.Contains(string(publisher.lastCall().payload), `"ligado":true`) {
		t.Errorf("payload = %s, want opaque passthrough", publisher.lastCall().payload)
	}
}

func TestPublishCommand_UnknownDevice(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "nope", Type: "set_led"})
	_, err := client.PublishCommand(context.Background(), req)
	if err == nil {
		t.Fatal("PublishCommand() error = nil, want unknown device rejection")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestPublishCommand_PublisherError(t *testing.T) {
	publisher := &fakePublisher{err: errors.New(`device "led-1" is disabled`)}
	ts := newTestServer(t, publisher, fakeStatus{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	params, _ := structpb.NewStruct(map[string]any{"on": true})
	req := connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "set_led", Parameters: params})
	_, err := client.PublishCommand(context.Background(), req)
	if err == nil {
		t.Fatal("PublishCommand() error = nil, want publisher error surfaced")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestGetStatus(t *testing.T) {
	startedAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	status := fakeStatus{snap: mqtt.Snapshot{
		StartedAt:            &startedAt,
		Started:              true,
		MQTTConnected:        true,
		Subscriptions:        4,
		AcceptedMessages:     10,
		RejectedMessages:     1,
		LocalRoutesPublished: 2,
		LocalRoutesFailed:    0,
		OutboxStored:         5,
		OutboxDiscarded:      1,
		OutboxFailed:         0,
	}}
	ts := newTestServer(t, &fakePublisher{}, status)
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)
	resp, err := client.GetStatus(context.Background(), connect.NewRequest(&apiv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus() error = %v", err)
	}
	if !resp.Msg.Started || !resp.Msg.MqttConnected || resp.Msg.Subscriptions != 4 {
		t.Errorf("response = %#v", resp.Msg)
	}
	if resp.Msg.AcceptedMessages != 10 || resp.Msg.RejectedMessages != 1 {
		t.Errorf("response = %#v", resp.Msg)
	}
	if resp.Msg.OutboxStored != 5 || resp.Msg.OutboxDiscarded != 1 {
		t.Errorf("response = %#v", resp.Msg)
	}
	if resp.Msg.StartedAt == nil || !resp.Msg.StartedAt.AsTime().Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", resp.Msg.StartedAt, startedAt)
	}
}
