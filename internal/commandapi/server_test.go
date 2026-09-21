package commandapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePublisher never touches MQTT - see internal/diagnostics/server_test.go
// for the same no-broker-needed testing convention this mirrors.
type fakePublisher struct {
	err            error
	lastDeviceID   string
	lastPayload    []byte
	publishedCount int
}

func (f *fakePublisher) PublishCommand(_ context.Context, deviceID string, payload []byte) error {
	f.publishedCount++
	f.lastDeviceID = deviceID
	f.lastPayload = payload
	return f.err
}

func testServer(t *testing.T, publisher CommandPublisher) *Server {
	t.Helper()
	server, err := New(Config{Address: "127.0.0.1:0", Credentials: Credentials{Username: "user", Password: "pass"}}, publisher, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server
}

func doRequest(server *Server, method, body string, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/commands", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.SetBasicAuth("user", "pass")
	}
	recorder := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestPublishRequiresAuth(t *testing.T) {
	server := testServer(t, &fakePublisher{})
	recorder := doRequest(server, http.MethodPost, `{"device_id":"led-1","type":"set_led","parameters":{"on":true}}`, false)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
}

func TestPublishRejectsWrongCredentials(t *testing.T) {
	server := testServer(t, &fakePublisher{})
	req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(`{}`))
	req.SetBasicAuth("user", "wrong-password")
	recorder := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
}

func TestPublishSuccess(t *testing.T) {
	publisher := &fakePublisher{}
	server := testServer(t, publisher)
	recorder := doRequest(server, http.MethodPost, `{"device_id":"led-1","type":"set_led","parameters":{"on":true}}`, true)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if publisher.publishedCount != 1 || publisher.lastDeviceID != "led-1" {
		t.Fatalf("publisher called = %d times, lastDeviceID = %q", publisher.publishedCount, publisher.lastDeviceID)
	}

	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "published" || response["device_id"] != "led-1" || response["command_id"] == "" {
		t.Errorf("response = %#v", response)
	}

	// The published payload must match docs/mqtt.md's command contract:
	// a UUID command_id (server-generated, never the caller's problem),
	// the requested type, and parameters passed through unchanged.
	var envelope map[string]any
	if err := json.Unmarshal(publisher.lastPayload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["command_id"] != response["command_id"] {
		t.Errorf("payload command_id = %v, want to match response %v", envelope["command_id"], response["command_id"])
	}
	if envelope["type"] != "set_led" {
		t.Errorf("payload type = %v", envelope["type"])
	}
	if params, ok := envelope["parameters"].(map[string]any); !ok || params["on"] != true {
		t.Errorf("payload parameters = %v", envelope["parameters"])
	}
}

func TestPublishDefaultsMissingParametersToEmptyObject(t *testing.T) {
	publisher := &fakePublisher{}
	server := testServer(t, publisher)
	recorder := doRequest(server, http.MethodPost, `{"device_id":"led-1","type":"set_led"}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(publisher.lastPayload, &envelope); err != nil {
		t.Fatal(err)
	}
	if params, ok := envelope["parameters"].(map[string]any); !ok || len(params) != 0 {
		t.Errorf("payload parameters = %v, want empty object", envelope["parameters"])
	}
}

func TestPublishRejectsMissingDeviceID(t *testing.T) {
	server := testServer(t, &fakePublisher{})
	recorder := doRequest(server, http.MethodPost, `{"type":"set_led","parameters":{}}`, true)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "device_id") {
		t.Errorf("status/body = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestPublishRejectsMissingType(t *testing.T) {
	server := testServer(t, &fakePublisher{})
	recorder := doRequest(server, http.MethodPost, `{"device_id":"led-1","parameters":{}}`, true)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "type") {
		t.Errorf("status/body = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestPublishRejectsMalformedJSON(t *testing.T) {
	server := testServer(t, &fakePublisher{})
	recorder := doRequest(server, http.MethodPost, `{not json`, true)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

func TestPublishSurfacesPublisherErrorAsBadRequest(t *testing.T) {
	publisher := &fakePublisher{err: errors.New(`unknown device "led-9"`)}
	server := testServer(t, publisher)
	recorder := doRequest(server, http.MethodPost, `{"device_id":"led-9","type":"set_led","parameters":{}}`, true)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "led-9") {
		t.Errorf("status/body = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestNewRejectsMissingCredentials(t *testing.T) {
	_, err := New(Config{Address: "127.0.0.1:0"}, &fakePublisher{}, nil)
	if err == nil {
		t.Fatal("New() with no credentials: want error")
	}
}

func TestNewRejectsNilPublisher(t *testing.T) {
	_, err := New(Config{Address: "127.0.0.1:0", Credentials: Credentials{Username: "u", Password: "p"}}, nil, nil)
	if err == nil {
		t.Fatal("New() with nil publisher: want error")
	}
}
