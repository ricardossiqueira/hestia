package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

func TestHealthzReflectsMQTTAvailability(t *testing.T) {
	server := testServer(t, mqtt.Snapshot{Started: true, MQTTConnected: true}, outbox.Snapshot{})
	recorder := httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("health response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers = %#v", recorder.Header())
	}

	server = testServer(t, mqtt.Snapshot{Started: true}, outbox.Snapshot{})
	recorder = httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "unavailable") {
		t.Errorf("unhealthy response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestStatusReturnsPayloadFreeOperationalData(t *testing.T) {
	started := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	oldest := started.Add(time.Minute)
	server := testServer(t, mqtt.Snapshot{
		StartedAt: startedPtr(started), Started: true, MQTTConnected: true, Subscriptions: 4,
		AcceptedMessages: 3, RejectedMessages: 2, LocalRoutesPublished: 1, LocalRoutesFailed: 1,
		OutboxStored: 3, OutboxDiscarded: 1, OutboxFailed: 1,
	}, outbox.Snapshot{Stats: outbox.Stats{Messages: 2, PayloadBytes: 42}, OldestEnqueuedAt: &oldest})
	recorder := httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if _, exists := response["payload"]; exists {
		t.Fatal("status exposed a payload")
	}
	if response["started_at"] == nil || response["mqtt"].(map[string]any)["subscriptions"] != float64(4) || response["outbox"].(map[string]any)["payload_bytes"] != float64(42) {
		t.Errorf("status = %#v", response)
	}
}

func TestStatusDoesNotExposeOutboxFailureDetails(t *testing.T) {
	server := testServerWithOutbox(t, mqtt.Snapshot{}, fakeOutbox{err: errors.New("sensitive storage detail")})
	recorder := httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "sensitive") {
		t.Errorf("status failure = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestDiagnosticsOnlyAllowsGETAndKnownEndpoints(t *testing.T) {
	server := testServer(t, mqtt.Snapshot{}, outbox.Snapshot{})
	recorder := httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodPost, "/status", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Errorf("method response = %d headers=%#v", recorder.Code, recorder.Header())
	}
	recorder = httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("unknown response = %d", recorder.Code)
	}
}

func TestNewRejectsUnsafeAddressAndDependencies(t *testing.T) {
	valid := config.Diagnostics{Address: "127.0.0.1:8080", RequestTimeout: config.Duration(time.Second)}
	if _, err := New(valid, nil, fakeOutbox{}); err == nil {
		t.Error("New accepted nil gateway")
	}
	if _, err := New(valid, fakeGateway{}, nil); err == nil {
		t.Error("New accepted nil outbox")
	}
	valid.Address = "0.0.0.0:8080"
	if _, err := New(valid, fakeGateway{}, fakeOutbox{}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("New unsafe address error = %v", err)
	}
}

func testServer(t *testing.T, gateway mqtt.Snapshot, queue outbox.Snapshot) *Server {
	t.Helper()
	return testServerWithOutbox(t, gateway, fakeOutbox{snapshot: queue})
}

func testServerWithOutbox(t *testing.T, gateway mqtt.Snapshot, queue OutboxSnapshotter) *Server {
	t.Helper()
	server, err := New(config.Diagnostics{Address: "127.0.0.1:8080", RequestTimeout: config.Duration(time.Second)}, fakeGateway{snapshot: gateway}, queue)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func startedPtr(value time.Time) *time.Time { return &value }

type fakeGateway struct{ snapshot mqtt.Snapshot }

func (gateway fakeGateway) Snapshot() mqtt.Snapshot { return gateway.snapshot }

type fakeOutbox struct {
	snapshot outbox.Snapshot
	err      error
}

func (queue fakeOutbox) Snapshot(context.Context) (outbox.Snapshot, error) {
	return queue.snapshot, queue.err
}
