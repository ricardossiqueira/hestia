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
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/admin"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
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

	provisionErr        error
	provisioned         config.Device
	password            string
	registerExistingErr error
	registeredExisting  config.Device
	cydProvisionErr     error
	cydProvisioned      config.Device
	cydAddress          string
	ledProvisionErr     error
	ledProvisioned      config.Device
	ledAddress          string

	setEnabledErr error
	setEnabled    config.Device

	removeErr    error
	removedID    string
	removedCalls int

	routes         []config.Route
	listRoutesErr  error
	createRouteErr error
	createdRoute   config.Route
	removeRouteErr error
	removedRouteID string

	manifests        []registry.DeviceManifest
	listManifestsErr error
	manifest         registry.DeviceManifest
	getManifestErr   error

	inconsistencies         []registry.Inconsistency
	listInconsistenciesErr  error
	resolveInconsistencyErr error
	resolvedInconsistencyID string
}

func (f *fakeDeviceAdmin) RegisterExistingDevice(ctx context.Context, id, template string) (config.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registeredExisting, f.registerExistingErr
}

func (f *fakeDeviceAdmin) ListRoutes(ctx context.Context) ([]config.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routes, f.listRoutesErr
}

func (f *fakeDeviceAdmin) CreateRoute(ctx context.Context, route config.Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdRoute = route
	return f.createRouteErr
}

func (f *fakeDeviceAdmin) RemoveRoute(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedRouteID = id
	return f.removeRouteErr
}

func (f *fakeDeviceAdmin) ListPublishedDeviceManifests(ctx context.Context) ([]registry.DeviceManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifests, f.listManifestsErr
}

func (f *fakeDeviceAdmin) GetPublishedDeviceManifest(ctx context.Context, id string) (registry.DeviceManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifest, f.getManifestErr
}

func (f *fakeDeviceAdmin) ProvisionDevice(ctx context.Context, id, template string) (config.Device, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return config.Device{}, "", f.provisionErr
	}
	return f.provisioned, f.password, nil
}

func (f *fakeDeviceAdmin) ProvisionCYD(ctx context.Context, id, address string) (config.Device, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cydProvisionErr != nil {
		return config.Device{}, "", f.cydProvisionErr
	}
	return f.cydProvisioned, f.cydAddress, nil
}

func (f *fakeDeviceAdmin) ProvisionLED(ctx context.Context, id, address string) (config.Device, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ledProvisionErr != nil {
		return config.Device{}, "", f.ledProvisionErr
	}
	return f.ledProvisioned, f.ledAddress, nil
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

func (f *fakeDeviceAdmin) ListInconsistencies(ctx context.Context) ([]registry.Inconsistency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inconsistencies, f.listInconsistenciesErr
}

func (f *fakeDeviceAdmin) ResolveInconsistency(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolvedInconsistencyID = id
	return f.resolveInconsistencyErr
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

func TestDeviceAdminService_ProvisionCYD_DoesNotExposeMQTTPassword(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	enabled := true
	fake := &fakeDeviceAdmin{
		cydProvisioned: config.Device{ID: "cyd-sala", Type: "esp32-cyd", Enabled: &enabled},
		cydAddress:     "192.168.15.42",
	}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	response, err := connectClient(ts).ProvisionCYD(context.Background(), authedRequest(&apiv1.ProvisionCYDRequest{
		DeviceId: "cyd-sala", DeviceIp: "192.168.15.42",
	}))
	if err != nil {
		t.Fatalf("ProvisionCYD() error = %v", err)
	}
	if response.Msg.GetDevice().GetId() != "cyd-sala" || response.Msg.GetDeviceIp() != "192.168.15.42" || response.Msg.GetAppliedAt() == nil {
		t.Fatalf("response = %#v", response.Msg)
	}
}

func TestDeviceAdminService_ProvisionCYD_MapsInvalidAddress(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{cydProvisionErr: admin.ErrInvalidDeviceAddress}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)
	_, err := connectClient(ts).ProvisionCYD(context.Background(), authedRequest(&apiv1.ProvisionCYDRequest{
		DeviceId: "cyd-sala", DeviceIp: "8.8.8.8",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_ProvisionLED_DoesNotExposeMQTTPassword(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	enabled := true
	fake := &fakeDeviceAdmin{
		ledProvisioned: config.Device{ID: "led-sala", Type: "esp32", Profile: "led.v1", Enabled: &enabled},
		ledAddress:     "192.168.15.43",
	}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)
	response, err := connectClient(ts).ProvisionLED(context.Background(), authedRequest(&apiv1.ProvisionLEDRequest{
		DeviceId: "led-sala", DeviceIp: "192.168.15.43",
	}))
	if err != nil {
		t.Fatalf("ProvisionLED() error = %v", err)
	}
	if response.Msg.GetDevice().GetId() != "led-sala" || response.Msg.GetDeviceIp() != "192.168.15.43" || response.Msg.GetAppliedAt() == nil {
		t.Fatalf("response = %#v", response.Msg)
	}
}

func TestDeviceAdminService_CreateAndListRoutes(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	route := config.Route{
		ID: "orangepi-to-monitor", SourceTopic: "devices/orangepi-monitor/telemetry", DestinationTopic: "devices/monitor/command",
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_system_status"}, QoS: 1,
	}
	fake := &fakeDeviceAdmin{routes: []config.Route{route}}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	list, err := connectClient(ts).ListRoutes(context.Background(), authedRequest(&apiv1.ListRoutesRequest{}))
	if err != nil {
		t.Fatalf("ListRoutes() error = %v", err)
	}
	if len(list.Msg.GetRoutes()) != 1 || list.Msg.GetRoutes()[0].GetDestinationTopic() != route.DestinationTopic {
		t.Fatalf("routes = %#v", list.Msg.GetRoutes())
	}
	created, err := connectClient(ts).CreateRoute(context.Background(), authedRequest(&apiv1.CreateRouteRequest{Route: &apiv1.Route{
		Id: route.ID, SourceTopic: route.SourceTopic, DestinationTopic: route.DestinationTopic,
		CommandType: route.Transform.CommandType, Qos: uint32(route.QoS), Retain: route.Retain,
	}}))
	if err != nil {
		t.Fatalf("CreateRoute() error = %v", err)
	}
	if created.Msg.GetAppliedAt() == nil || fake.createdRoute != route {
		t.Fatalf("response = %#v, route = %#v", created.Msg, fake.createdRoute)
	}
}

func TestDeviceAdminService_ListAndGetPublishedDeviceManifests(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	createdAt := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	manifest := registry.DeviceManifest{
		ID: "esp32-c3-led", DisplayName: "ESP32-C3 LED", Revision: 1,
		Document: `{"schema_version":1}`, CreatedBy: "system:bootstrap", CreatedAt: createdAt,
	}
	fake := &fakeDeviceAdmin{manifests: []registry.DeviceManifest{manifest}, manifest: manifest}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	list, err := connectClient(ts).ListDeviceManifests(context.Background(), authedRequest(&apiv1.ListDeviceManifestsRequest{}))
	if err != nil {
		t.Fatalf("ListDeviceManifests() error = %v", err)
	}
	if len(list.Msg.GetManifests()) != 1 || list.Msg.GetManifests()[0].GetId() != manifest.ID || list.Msg.GetManifests()[0].GetDocumentJson() != manifest.Document {
		t.Fatalf("list = %#v", list.Msg)
	}
	got, err := connectClient(ts).GetDeviceManifest(context.Background(), authedRequest(&apiv1.GetDeviceManifestRequest{ManifestId: manifest.ID}))
	if err != nil {
		t.Fatalf("GetDeviceManifest() error = %v", err)
	}
	if got.Msg.GetManifest().GetCreatedAt().AsTime() != createdAt || got.Msg.GetManifest().GetCreatedBy() != manifest.CreatedBy {
		t.Fatalf("get = %#v", got.Msg)
	}
}

func TestDeviceAdminService_GetDeviceManifestMapsNotFound(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, &fakeDeviceAdmin{getManifestErr: registry.ErrManifestNotFound})
	_, err := connectClient(ts).GetDeviceManifest(context.Background(), authedRequest(&apiv1.GetDeviceManifestRequest{ManifestId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestDeviceAdminService_RegisterExistingDevice(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	enabled := true
	fake := &fakeDeviceAdmin{registeredExisting: config.Device{
		ID: "orangepi-monitor", Type: "linux-system-monitor", Enabled: &enabled,
		Topics: config.Topics{Telemetry: "devices/orangepi-monitor/telemetry"},
	}}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)
	response, err := connectClient(ts).RegisterExistingDevice(context.Background(), authedRequest(&apiv1.RegisterExistingDeviceRequest{
		DeviceId: "orangepi-monitor", Template: "orangepi_monitor.v1",
	}))
	if err != nil {
		t.Fatalf("RegisterExistingDevice() error = %v", err)
	}
	if response.Msg.GetAppliedAt() == nil || response.Msg.GetDevice().GetTopics().GetTelemetry() != "devices/orangepi-monitor/telemetry" {
		t.Fatalf("response = %#v", response.Msg)
	}
}

func TestDeviceAdminService_RemoveRouteNotFound(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, &fakeDeviceAdmin{removeRouteErr: admin.ErrRouteNotFound})
	_, err := connectClient(ts).RemoveRoute(context.Background(), authedRequest(&apiv1.RemoveRouteRequest{RouteId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
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

func TestDeviceAdminService_ListInconsistencies(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	createdAt := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	fake := &fakeDeviceAdmin{inconsistencies: []registry.Inconsistency{{
		ID: "abc123", Kind: "remove_device", DeviceID: "led-1",
		Cause: "Mosquitto credential already revoked", CompensationError: "registry removal failed: disk full",
		CreatedAt: createdAt,
	}}}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	resp, err := connectClient(ts).ListInconsistencies(context.Background(), authedRequest(&apiv1.ListInconsistenciesRequest{}))
	if err != nil {
		t.Fatalf("ListInconsistencies() error = %v", err)
	}
	items := resp.Msg.GetInconsistencies()
	if len(items) != 1 {
		t.Fatalf("inconsistencies = %#v, want 1", items)
	}
	if items[0].GetId() != "abc123" || items[0].GetDeviceId() != "led-1" || items[0].GetKind() != "remove_device" {
		t.Errorf("entry = %#v", items[0])
	}
	if items[0].GetCreatedAt() == nil || !items[0].GetCreatedAt().AsTime().Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", items[0].GetCreatedAt(), createdAt)
	}
}

func TestDeviceAdminService_ResolveInconsistency_Success(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).ResolveInconsistency(context.Background(), authedRequest(&apiv1.ResolveInconsistencyRequest{Id: "abc123"}))
	if err != nil {
		t.Fatalf("ResolveInconsistency() error = %v", err)
	}
	if fake.resolvedInconsistencyID != "abc123" {
		t.Errorf("resolvedInconsistencyID = %q, want abc123", fake.resolvedInconsistencyID)
	}
}

func TestDeviceAdminService_ResolveInconsistency_NotFound(t *testing.T) {
	backend, _ := newFakeInternalAPI(t)
	fake := &fakeDeviceAdmin{resolveInconsistencyErr: admin.ErrInconsistencyNotFound}
	ts := newTestGatewayWithAdmin(t, backend.URL, nil, fake)

	_, err := connectClient(ts).ResolveInconsistency(context.Background(), authedRequest(&apiv1.ResolveInconsistencyRequest{Id: "ghost"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}
