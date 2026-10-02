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
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
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

func (f *fakePublisher) PublishRaw(ctx context.Context, topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, publishCall{deviceID: topic, payload: append([]byte(nil), payload...)})
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

// newTestServer starts an httptest server directly on the Server's
// http.Handler (bypassing Start/net.Listen, which would bind a real port)
// so tests stay fast and hermetic. This is safe because server_test.go
// lives in package api and can reach the unexported s.http field. No auth
// is exercised here any more (ADR-013): this package is loopback-only and
// trusts its caller (internal/apigateway's reverse proxy) unconditionally.
// events is variadic so every existing call site stays unchanged - most
// tests here don't care about the activity log at all.
func newTestServer(t *testing.T, publisher *fakePublisher, status fakeStatus, queue fakeQueue, events ...mqtt.ActivityEvent) *httptest.Server {
	t.Helper()
	cfg := Config{Address: "127.0.0.1:0", RequestTimeout: time.Second}
	srv, err := New(cfg, publisher, status, queue, &fakeEvents{events: events}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleV2PublishCommand_Success(t *testing.T) {
	publisher := &fakePublisher{}
	ts := newTestServer(t, publisher, fakeStatus{}, fakeQueue{})
	body := `{"topic":"devices/led-sala/command","payload":{"command_id":"5c1f7b1e-3b7e-4a1a-8f4a-5a5c1f7b1e3b","type":"set_led","parameters":{"on":true}}}`
	resp, err := ts.Client().Post(ts.URL+"/internal/v2/publish-command", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if publisher.callCount() != 1 {
		t.Fatalf("publisher calls = %d, want 1", publisher.callCount())
	}
	if call := publisher.lastCall(); call.deviceID != "devices/led-sala/command" {
		t.Errorf("topic = %q", call.deviceID)
	}
}

func TestHandleV2PublishCommand_MissingTopicIsBadRequest(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{})
	resp, err := ts.Client().Post(ts.URL+"/internal/v2/publish-command", "application/json", strings.NewReader(`{"payload":{"a":1}}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleV2PublishCommand_PublisherErrorIsBadGateway(t *testing.T) {
	publisher := &fakePublisher{err: errors.New("broker unreachable")}
	ts := newTestServer(t, publisher, fakeStatus{}, fakeQueue{})
	resp, err := ts.Client().Post(ts.URL+"/internal/v2/publish-command", "application/json", strings.NewReader(`{"topic":"devices/led-sala/command","payload":{"a":1}}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
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
	ts := newTestServer(t, &fakePublisher{}, status, fakeQueue{})
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

func TestGetQueueSummaryEmpty(t *testing.T) {
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{snap: outbox.Snapshot{}})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{snap: snap})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{err: errors.New("outbox unavailable")})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{})
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
	ts := newTestServer(t, &fakePublisher{}, fakeStatus{}, fakeQueue{},
		mqtt.ActivityEvent{Timestamp: when, DeviceID: "led-1", Kind: "state", Topic: "devices/led-1/state", Outcome: "accepted"},
		mqtt.ActivityEvent{Timestamp: when, DeviceID: "led-1", Topic: "devices/display/command", Outcome: "v2_rule_fired", Detail: "status-to-display"},
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
	if events[1].GetOutcome() != "v2_rule_fired" || events[1].GetDetail() != "status-to-display" || events[1].GetKind() != "" {
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
	cfg := Config{Address: "127.0.0.1:0", RequestTimeout: time.Second}
	srv, err := New(cfg, &fakePublisher{}, fakeStatus{}, fakeQueue{}, events, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}
