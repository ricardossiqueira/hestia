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
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
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

// fakeTelemetry is keyed by device ID; a missing key means "not received
// yet", the same as internal/mqtt.Gateway.LastTelemetry's ok=false.
type fakeTelemetry map[string]struct {
	payload    []byte
	observedAt time.Time
}

func (f fakeTelemetry) LastTelemetry(deviceID string) ([]byte, time.Time, bool) {
	entry, ok := f[deviceID]
	if !ok {
		return nil, time.Time{}, false
	}
	return entry.payload, entry.observedAt, true
}

type fakeQueue struct {
	snap outbox.Snapshot
	err  error
}

func (f fakeQueue) Snapshot(context.Context) (outbox.Snapshot, error) { return f.snap, f.err }

// fakeEvents is a pointer so a test can hold onto it after newTestServer
// returns and assert on lastFilter (what GetRecentEvents actually passed
// through) and control hasMore independently of len(events).
type fakeEvents struct {
	mu         sync.Mutex
	events     []mqtt.ActivityEvent
	hasMore    bool
	lastFilter mqtt.EventFilter
}

func (f *fakeEvents) RecentEvents(filter mqtt.EventFilter) ([]mqtt.ActivityEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	return f.events, f.hasMore
}

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

type fakeManifestResolver struct {
	document devicemanifest.Document
	found    bool
	err      error
}

func (f fakeManifestResolver) ResolveDeviceManifest(context.Context, string) (devicemanifest.Document, bool, error) {
	return f.document, f.found, f.err
}

// newTestServer starts an httptest server directly on the Server's
// http.Handler (bypassing Start/net.Listen, which would bind a real port)
// so tests stay fast and hermetic. This is safe because server_test.go
// lives in package api and can reach the unexported s.http field. No auth
// is exercised here any more (ADR-013): this package is loopback-only and
// trusts its caller (internal/apigateway's reverse proxy) unconditionally.
// events is variadic so every existing call site (there are many) stays
// unchanged - most tests here don't care about the activity log at all.
func newTestServer(t *testing.T, publisher *fakePublisher, status fakeStatus, telemetry fakeTelemetry, queue fakeQueue, events ...mqtt.ActivityEvent) *httptest.Server {
	t.Helper()
	cfg := Config{
		Address:        "127.0.0.1:0",
		RequestTimeout: time.Second,
		Registry:       config.Config{Devices: testDevices()},
	}
	srv, err := New(cfg, publisher, status, telemetry, queue, &fakeEvents{events: events}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestListDevices(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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

func TestPublishCommand_UsesBoundManifest(t *testing.T) {
	publisher := &fakePublisher{}
	document, _, err := devicemanifest.Parse(`{"schema_version":1,"id":"led","display_name":"LED","provisioning":{"protocol":"http-nvs-v1","model":"esp32","required_protocol_version":1},"mqtt":{"topics":["command"]},"capabilities":{"commands":[{"type":"blink","parameters":{"times":{"type":"integer","required":true}}}],"events":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Address: "127.0.0.1:0", RequestTimeout: time.Second, Registry: config.Config{Devices: testDevices()}, ManifestResolver: fakeManifestResolver{document: document, found: true}}
	srv, err := New(cfg, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{}, &fakeEvents{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)
	params, _ := structpb.NewStruct(map[string]any{"times": 3})
	response, err := client.PublishCommand(context.Background(), connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "blink", Parameters: params}))
	if err != nil || !response.Msg.GetSchemaValidated() || publisher.callCount() != 1 {
		t.Fatalf("response=%#v err=%v calls=%d", response.Msg, err, publisher.callCount())
	}
	_, err = client.PublishCommand(context.Background(), connect.NewRequest(&apiv1.PublishCommandRequest{DeviceId: "led-1", Type: "set_led"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code=%v", connect.CodeOf(err))
	}
}

func TestPublishCommand_Success_SchemaValidated(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, publisher, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
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
	ts := newTestServer(t, &fakePublisher{}, status, fakeTelemetry{}, fakeQueue{})
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

func TestGetDeviceTelemetryReturnsCachedPayload(t *testing.T) {
	observedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	telemetry := fakeTelemetry{"led-1": {payload: []byte(`{"cpu_pct":12.5}`), observedAt: observedAt}}
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, telemetry, fakeQueue{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetDeviceTelemetry(context.Background(), connect.NewRequest(&apiv1.GetDeviceTelemetryRequest{DeviceId: "led-1"}))
	if err != nil {
		t.Fatalf("GetDeviceTelemetry() error = %v", err)
	}
	if !resp.Msg.Available {
		t.Fatal("Available = false, want true")
	}
	if got := resp.Msg.Payload.AsMap()["cpu_pct"]; got != 12.5 {
		t.Errorf("Payload[cpu_pct] = %v, want 12.5", got)
	}
	if resp.Msg.ObservedAt == nil || !resp.Msg.ObservedAt.AsTime().Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v", resp.Msg.ObservedAt, observedAt)
	}
}

func TestGetDeviceTelemetryAvailableFalseWhenNothingReceivedYet(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetDeviceTelemetry(context.Background(), connect.NewRequest(&apiv1.GetDeviceTelemetryRequest{DeviceId: "led-1"}))
	if err != nil {
		t.Fatalf("GetDeviceTelemetry() error = %v", err)
	}
	if resp.Msg.Available {
		t.Fatal("Available = true, want false when no telemetry was cached")
	}
}

func TestGetDeviceTelemetryRejectsUnknownDevice(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
	client := apiv1connect.NewDeviceServiceClient(ts.Client(), ts.URL)

	_, err := client.GetDeviceTelemetry(context.Background(), connect.NewRequest(&apiv1.GetDeviceTelemetryRequest{DeviceId: "ghost"}))
	if err == nil {
		t.Fatal("GetDeviceTelemetry() error = nil, want unknown device rejected")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestGetQueueSummaryEmpty(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{snap: outbox.Snapshot{}})
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetQueueSummary(context.Background(), connect.NewRequest(&apiv1.GetQueueSummaryRequest{}))
	if err != nil {
		t.Fatalf("GetQueueSummary() error = %v", err)
	}
	if resp.Msg.PendingMessages != 0 || resp.Msg.PendingBytes != 0 {
		t.Errorf("response = %#v, want zero", resp.Msg)
	}
	if resp.Msg.OldestEnqueuedAt != nil {
		t.Errorf("OldestEnqueuedAt = %v, want unset for an empty queue", resp.Msg.OldestEnqueuedAt)
	}
}

func TestGetQueueSummaryWithPendingMessages(t *testing.T) {
	oldest := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	snap := outbox.Snapshot{
		Stats:            outbox.Stats{Messages: 3, PayloadBytes: 512},
		OldestEnqueuedAt: &oldest,
	}
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{snap: snap})
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetQueueSummary(context.Background(), connect.NewRequest(&apiv1.GetQueueSummaryRequest{}))
	if err != nil {
		t.Fatalf("GetQueueSummary() error = %v", err)
	}
	if resp.Msg.PendingMessages != 3 || resp.Msg.PendingBytes != 512 {
		t.Errorf("response = %#v", resp.Msg)
	}
	if resp.Msg.OldestEnqueuedAt == nil || !resp.Msg.OldestEnqueuedAt.AsTime().Equal(oldest) {
		t.Errorf("OldestEnqueuedAt = %v, want %v", resp.Msg.OldestEnqueuedAt, oldest)
	}
}

func TestGetQueueSummaryPropagatesStoreError(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{err: errors.New("outbox unavailable")})
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	_, err := client.GetQueueSummary(context.Background(), connect.NewRequest(&apiv1.GetQueueSummaryRequest{}))
	if err == nil {
		t.Fatal("GetQueueSummary() error = nil, want the store error surfaced")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}

func TestGetRecentEventsEmpty(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{})
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetRecentEvents(context.Background(), connect.NewRequest(&apiv1.GetRecentEventsRequest{}))
	if err != nil {
		t.Fatalf("GetRecentEvents() error = %v", err)
	}
	if len(resp.Msg.GetEvents()) != 0 {
		t.Errorf("events = %#v, want empty", resp.Msg.GetEvents())
	}
}

func TestGetRecentEventsReturnsGatewayActivity(t *testing.T) {
	when := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{},
		mqtt.ActivityEvent{Timestamp: when, DeviceID: "led-1", Kind: "state", Topic: "devices/led-1/state", Outcome: "accepted"},
		mqtt.ActivityEvent{Timestamp: when, DeviceID: "led-1", Topic: "devices/display/command", Outcome: "route_published", Detail: "status-to-display"},
	)
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetRecentEvents(context.Background(), connect.NewRequest(&apiv1.GetRecentEventsRequest{}))
	if err != nil {
		t.Fatalf("GetRecentEvents() error = %v", err)
	}
	events := resp.Msg.GetEvents()
	if len(events) != 2 {
		t.Fatalf("events = %#v, want 2", events)
	}
	if events[0].GetDeviceId() != "led-1" || events[0].GetOutcome() != "accepted" || events[0].GetKind() != "state" {
		t.Errorf("events[0] = %#v", events[0])
	}
	if events[0].GetTimestamp() == nil || !events[0].GetTimestamp().AsTime().Equal(when) {
		t.Errorf("events[0].Timestamp = %v, want %v", events[0].GetTimestamp(), when)
	}
	if events[1].GetOutcome() != "route_published" || events[1].GetDetail() != "status-to-display" || events[1].GetKind() != "" {
		t.Errorf("events[1] = %#v", events[1])
	}
}

func TestGetRecentEventsPassesFilterThrough(t *testing.T) {
	since := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	events := &fakeEvents{}
	ts := newTestServerWithEvents(t, events)
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	_, err := client.GetRecentEvents(context.Background(), connect.NewRequest(&apiv1.GetRecentEventsRequest{
		DeviceId: "led-1", Since: timestamppb.New(since), Limit: 10, BeforeSequence: 42,
	}))
	if err != nil {
		t.Fatalf("GetRecentEvents() error = %v", err)
	}
	got := events.lastFilter
	if got.DeviceID != "led-1" || got.Limit != 10 || got.BeforeSequence != 42 || !got.Since.Equal(since) {
		t.Errorf("lastFilter = %#v, want device led-1, limit 10, beforeSequence 42, since %v", got, since)
	}
}

func TestGetRecentEventsSurfacesHasMore(t *testing.T) {
	events := &fakeEvents{
		events:  []mqtt.ActivityEvent{{DeviceID: "led-1", Outcome: "accepted", Timestamp: time.Now().UTC()}},
		hasMore: true,
	}
	ts := newTestServerWithEvents(t, events)
	client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)

	resp, err := client.GetRecentEvents(context.Background(), connect.NewRequest(&apiv1.GetRecentEventsRequest{}))
	if err != nil {
		t.Fatalf("GetRecentEvents() error = %v", err)
	}
	if !resp.Msg.GetHasMore() {
		t.Error("HasMore = false, want true")
	}
}

// newTestServerWithEvents mirrors newTestServer but takes a pre-built
// *fakeEvents directly, so a test can inspect it (lastFilter, hasMore)
// after the call - the variadic form on newTestServer can't do that since
// it builds its own *fakeEvents internally.
func newTestServerWithEvents(t *testing.T, events *fakeEvents) *httptest.Server {
	t.Helper()
	cfg := Config{
		Address:        "127.0.0.1:0",
		RequestTimeout: time.Second,
		Registry:       config.Config{Devices: testDevices()},
	}
	srv, err := New(cfg, &fakePublisher{}, fakeStatus{}, fakeTelemetry{}, fakeQueue{}, events, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}
