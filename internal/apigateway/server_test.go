package apigateway

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/admin"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
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

// fakeDeviceAdmin is DeviceAdmin's test double, mirroring fakePublisher's
// shape in internal/api's own tests: records calls, and its three fields
// let a test force each method to fail with a specific error.
type fakeDeviceAdmin struct {
	mu sync.Mutex

	provisionErr error
	provisioned  config.Device
	password     string

	setEnabledErr error
	setEnabled    config.Device

	removeErr    error
	removedID    string
	removedCalls int
}

func (f *fakeDeviceAdmin) ProvisionDevice(ctx context.Context, id, template string) (config.Device, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return config.Device{}, "", f.provisionErr
	}
	return f.provisioned, f.password, nil
}

func (f *fakeDeviceAdmin) SetDeviceEnabled(ctx context.Context, id string, enabled bool) (config.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setEnabledErr != nil {
		return config.Device{}, f.setEnabledErr
	}
	return f.setEnabled, nil
}

func (f *fakeDeviceAdmin) RemoveDevice(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedID = id
	f.removedCalls++
	return f.removeErr
}

func newTestGateway(t *testing.T, backendURL string, allowedOrigins []string) *httptest.Server {
	t.Helper()
	return newTestGatewayWithAdmin(t, backendURL, allowedOrigins, &fakeDeviceAdmin{})
}

func newTestGatewayWithAdmin(t *testing.T, backendURL string, allowedOrigins []string, deviceAdmin DeviceAdmin) *httptest.Server {
	t.Helper()
	srv, err := New(Config{
		Address:        "127.0.0.1:0",
		InternalAPIURL: backendURL,
		Credentials:    Credentials{Username: "user", Password: "pass"},
		AllowedOrigins: allowedOrigins,
		Admin:          deviceAdmin,
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
	resp, err := http.Post(ts.URL+"/iot.gateway.api.v1.DeviceService/ListDevices", "application/json", strings.NewReader("{}"))
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
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.DeviceService/ListDevices", strings.NewReader("{}"))
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

// DeviceAdminService needs auth too, even though it is answered directly
// (not proxied) - same mux, same middleware chain as DeviceService/
// GatewayService, but worth a dedicated assertion since it is a different
// code path (a Connect handler, not httputil.ReverseProxy).
func TestUnauthorized_DeviceAdminService(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.DeviceAdminService/ProvisionDevice", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProxy_DeviceService_ForwardsIntact(t *testing.T) {
	backend, rec := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.DeviceService/PublishCommand", strings.NewReader(`{"deviceId":"led-1"}`))
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
	if rec.path != "/iot.gateway.api.v1.DeviceService/PublishCommand" {
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

func TestProxy_GatewayService_Forwards(t *testing.T) {
	backend, rec := newFakeInternalAPI(t)
	ts := newTestGateway(t, backend.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/iot.gateway.api.v1.GatewayService/GetStatus", strings.NewReader("{}"))
	req.Header.Set("Authorization", authHeader("user", "pass"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rec.path != "/iot.gateway.api.v1.GatewayService/GetStatus" {
		t.Errorf("backend saw path = %q", rec.path)
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
	if _, err := New(Config{
		Address: "127.0.0.1:0", InternalAPIURL: "http://127.0.0.1:8083",
		Credentials: Credentials{Username: "u", Password: "p"},
	}, nil); err == nil {
		t.Error("New() without Admin: want error")
	}
}

func connectClient(ts *httptest.Server) apiv1connect.DeviceAdminServiceClient {
	return apiv1connect.NewDeviceAdminServiceClient(ts.Client(), ts.URL)
}

func authedRequest[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", authHeader("user", "pass"))
	return req
}

func TestDeviceAdminService_ProvisionDevice_Success(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	enabled := true
	fake := &fakeDeviceAdmin{
		provisioned: config.Device{ID: "led-1", Type: "esp32", Profile: "led.v1", Enabled: &enabled},
		password:    "generated-secret",
	}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	resp, err := connectClient(ts).ProvisionDevice(context.Background(), authedRequest(&apiv1.ProvisionDeviceRequest{
		DeviceId: "led-1", Template: "esp32_led.v1",
	}))
	if err != nil {
		t.Fatalf("ProvisionDevice() error = %v", err)
	}
	if resp.Msg.MqttUsername != "led-1" || resp.Msg.MqttPassword != "generated-secret" {
		t.Errorf("response = %#v", resp.Msg)
	}
	if resp.Msg.Device.GetProfile() != "led.v1" {
		t.Errorf("device = %#v", resp.Msg.Device)
	}
	if resp.Msg.AppliedAt == nil {
		t.Error("AppliedAt is nil")
	}
}

func TestDeviceAdminService_ProvisionDevice_AlreadyExists(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{provisionErr: admin.ErrDeviceAlreadyExists}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).ProvisionDevice(context.Background(), authedRequest(&apiv1.ProvisionDeviceRequest{
		DeviceId: "led-1", Template: "esp32_led.v1",
	}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("code = %v, want AlreadyExists", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_ProvisionDevice_InvalidArgument(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{provisionErr: admin.ErrUnknownTemplate}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).ProvisionDevice(context.Background(), authedRequest(&apiv1.ProvisionDeviceRequest{
		DeviceId: "led-1", Template: "no-such-template",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_ProvisionDevice_Internal(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{provisionErr: errors.New("provisioning script failed")}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).ProvisionDevice(context.Background(), authedRequest(&apiv1.ProvisionDeviceRequest{
		DeviceId: "led-1", Template: "esp32_led.v1",
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_SetDeviceEnabled_Success(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	enabled := false
	fake := &fakeDeviceAdmin{setEnabled: config.Device{ID: "led-1", Enabled: &enabled}}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	resp, err := connectClient(ts).SetDeviceEnabled(context.Background(), authedRequest(&apiv1.SetDeviceEnabledRequest{
		DeviceId: "led-1", Enabled: false,
	}))
	if err != nil {
		t.Fatalf("SetDeviceEnabled() error = %v", err)
	}
	if resp.Msg.Device.GetEnabled() {
		t.Error("Device.Enabled = true, want false")
	}
}

func TestDeviceAdminService_SetDeviceEnabled_NotFound(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{setEnabledErr: admin.ErrDeviceNotFound}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).SetDeviceEnabled(context.Background(), authedRequest(&apiv1.SetDeviceEnabledRequest{
		DeviceId: "no-such-device", Enabled: true,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_RemoveDevice_Success(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	resp, err := connectClient(ts).RemoveDevice(context.Background(), authedRequest(&apiv1.RemoveDeviceRequest{
		DeviceId: "led-1",
	}))
	if err != nil {
		t.Fatalf("RemoveDevice() error = %v", err)
	}
	if resp.Msg.AppliedAt == nil {
		t.Error("AppliedAt is nil")
	}
	if fake.removedCalls != 1 || fake.removedID != "led-1" {
		t.Errorf("removedCalls = %d, removedID = %q", fake.removedCalls, fake.removedID)
	}
}

func TestDeviceAdminService_RemoveDevice_NotFound(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{removeErr: admin.ErrDeviceNotFound}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).RemoveDevice(context.Background(), authedRequest(&apiv1.RemoveDeviceRequest{
		DeviceId: "no-such-device",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_RemoveDevice_Internal(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{removeErr: errors.New("systemctl restart failed")}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).RemoveDevice(context.Background(), authedRequest(&apiv1.RemoveDeviceRequest{
		DeviceId: "led-1",
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}
