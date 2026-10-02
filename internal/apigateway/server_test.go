package apigateway

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func authHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// newFakeInternalAPI starts a backend that records the last request it
// received (method, path, headers, body) and echoes a small JSON body back,
// standing in for internal/api during these tests.
type recordedRequest struct {
	method string
	path   string
	body   string
	auth   string
}

func newFakeInternalAPI(t *testing.T) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.body = string(body)
		rec.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	return backend, rec
}

func newTestGateway(t *testing.T, backendURL string, allowedOrigins []string) *httptest.Server {
	t.Helper()
	srv, err := New(Config{
		Address:        "127.0.0.1:0",
		InternalAPIURL: backendURL,
		Credentials:    Credentials{Username: "user", Password: "pass"},
		AllowedOrigins: allowedOrigins,
	}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestUnauthorized_MissingCredentials(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	resp, err := http.Post(ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header")
	}
}

func TestUnauthorized_WrongCredentials(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader("{}"))
	req.Header.Set("Authorization", authHeader("user", "wrong-password"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProxy_GatewayService_Forwards(t *testing.T) {
	backend, rec := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader(`{"deviceId":"led-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader("user", "pass"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s, want the backend's response forwarded back", body)
	}
	if rec.method != http.MethodPost {
		t.Errorf("backend saw method = %q, want POST", rec.method)
	}
	if rec.path != "/iot.gateway.api.v1.GatewayService/GetStatus" {
		t.Errorf("backend saw path = %q", rec.path)
	}
	if rec.body != `{"deviceId":"led-1"}` {
		t.Errorf("backend saw body = %q, want it forwarded unchanged", rec.body)
	}
	// The border already authenticated this request; there is no reason to
	// strip Authorization before forwarding, even though internal/api
	// itself ignores it (loopback is its trust boundary).
	if rec.auth != authHeader("user", "pass") {
		t.Errorf("backend saw Authorization = %q, want it forwarded", rec.auth)
	}
}

func TestProxy_BackendUnreachable(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	backendURL := backend.URL
	backend.Close() // nothing listens here any more

	ts := newTestGateway(t, backendURL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader("{}"))
	req.Header.Set("Authorization", authHeader("user", "pass"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestCORS_DisabledByDefault(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Authorization", authHeader("user", "pass"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty (CORS disabled)", got)
	}
}

func TestCORS_PreflightAllowedOrigin(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, []string{"http://localhost:5173"})
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request's own origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestCORS_PreflightDisallowedOrigin(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, []string{"http://localhost:5173"})
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for a disallowed origin", got)
	}
}

func TestCORS_ActualRequestAllowedOrigin(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, []string{"http://localhost:5173"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Authorization", authHeader("user", "pass"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request's own origin", got)
	}
}

func TestNew_RequiresFields(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil {
		t.Error("New() with empty Config: want error")
	}
	if _, err := New(Config{Address: "127.0.0.1:0"}, nil); err == nil {
		t.Error("New() without InternalAPIURL: want error")
	}
	if _, err := New(Config{Address: "127.0.0.1:0", InternalAPIURL: "http://127.0.0.1:8083"}, nil); err == nil {
		t.Error("New() without Credentials: want error")
	}
}
