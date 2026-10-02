package apigateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

const apiV2Manifest = `{"schema_version":2,"manifest_id":"test-led","display_name":"Test LED","model":"test-led","protocol_version":1,"mqtt":{"publish":[{"channel":"state","retained":true,"schema":{}}],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{}}]}]}}`

type v2RegistrarFake struct {
	entry  devicev2.DiscoveryEntry
	device registry.V2Device
}

func (f *v2RegistrarFake) Register(_ context.Context, entry devicev2.DiscoveryEntry, deviceID string) (registry.V2Device, error) {
	f.entry = entry
	f.device.DeviceID = deviceID
	return f.device, nil
}

func TestDeviceV2APIRegistersPairingDiscoveryThroughRegistrar(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, canonical, hash, err := devicev2.Parse(apiV2Manifest)
	if err != nil {
		t.Fatal(err)
	}
	inbox := devicev2.NewInbox(0)
	announcement := devicev2.Announcement{DeviceUID: "test-uid", Host: "192.0.2.10", Port: 8080, Model: "test-led", Protocol: "iot-device-v1", Firmware: "2.0.0", ManifestSHA256: hash, Pairing: true}
	if err := inbox.Observe(announcement); err != nil {
		t.Fatal(err)
	}
	inbox.MarkInspected("test-uid", devicev2.DeviceInfo{DeviceUID: "test-uid", Model: "test-led", FirmwareVersion: "2.0.0", Manifest: json.RawMessage(canonical), ManifestSHA256: hash, IdentityPublicKey: "public", PairingRequired: true}, nil)
	registrar := &v2RegistrarFake{device: registry.V2Device{DeviceUID: "test-uid", ManifestID: "test-led", ManifestRevision: 1, ManifestSHA256: hash, FirmwareVersion: "2.0.0", IdentityPublicKey: "public", DesiredState: "active", ActiveState: "active"}}
	api := DeviceV2API{Inbox: inbox, Registry: store, Registrar: registrar}
	list := httptest.NewRecorder()
	api.ServeHTTP(list, httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/ListDiscovery", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/RegisterDiscoveredDevice", strings.NewReader(`{"deviceUid":"test-uid","deviceId":"led-sala"}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", response.Code, response.Body.String())
	}
	if registrar.entry.DeviceUID != "test-uid" || registrar.device.DeviceID != "led-sala" {
		t.Fatalf("registrar entry=%#v device=%#v", registrar.entry, registrar.device)
	}
	if !strings.Contains(response.Body.String(), `"activeState":"active"`) {
		t.Fatalf("register body=%s", response.Body.String())
	}
}

const apiV2LEDManifest = `{"schema_version":2,"manifest_id":"test-led-cmd","display_name":"Test LED","model":"test-led-cmd","protocol_version":1,"mqtt":{"publish":[],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}]}]}}`

type v2CommandPublisherFake struct {
	topic   string
	payload []byte
	err     error
}

func (f *v2CommandPublisherFake) Publish(_ context.Context, topic string, payload []byte) error {
	if f.err != nil {
		return f.err
	}
	f.topic, f.payload = topic, append([]byte(nil), payload...)
	return nil
}

func registerTestV2Device(t *testing.T, store *registry.Store, deviceID string) {
	t.Helper()
	manifest, err := store.AcceptV2Manifest(context.Background(), apiV2LEDManifest, "lab-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterV2Device(context.Background(), registry.V2Device{
		DeviceID: deviceID, DeviceUID: "uid-" + deviceID, ManifestID: manifest.ManifestID,
		ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256,
		FirmwareVersion: "0.1.0", IdentityPublicKey: "public-key",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceV2APIPublishCommandSendsValidatedEnvelope(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registerTestV2Device(t, store, "led-sala")
	publisher := &v2CommandPublisherFake{}
	api := DeviceV2API{Registry: store, CommandPublisher: publisher}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/PublishCommand", strings.NewReader(`{"deviceId":"led-sala","type":"set_led","parameters":{"on":true}}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if publisher.topic != "devices/led-sala/command" {
		t.Errorf("topic=%q", publisher.topic)
	}
	if !strings.Contains(string(publisher.payload), `"on":true`) {
		t.Errorf("payload=%s", publisher.payload)
	}
	if !strings.Contains(response.Body.String(), `"commandId"`) {
		t.Errorf("response body=%s", response.Body.String())
	}
}

func TestDeviceV2APIPublishCommandRejectsUndeclaredCommand(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registerTestV2Device(t, store, "led-sala")
	publisher := &v2CommandPublisherFake{}
	api := DeviceV2API{Registry: store, CommandPublisher: publisher}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/PublishCommand", strings.NewReader(`{"deviceId":"led-sala","type":"reboot","parameters":{}}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if publisher.topic != "" {
		t.Fatalf("publisher should not have been called, got topic=%q", publisher.topic)
	}
}

func TestDeviceV2APIPublishCommandRejectsInvalidParameters(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registerTestV2Device(t, store, "led-sala")
	publisher := &v2CommandPublisherFake{}
	api := DeviceV2API{Registry: store, CommandPublisher: publisher}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/PublishCommand", strings.NewReader(`{"deviceId":"led-sala","type":"set_led","parameters":{}}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if publisher.topic != "" {
		t.Fatalf("publisher should not have been called, got topic=%q", publisher.topic)
	}
}

func TestDeviceV2APIPublishCommandUnknownDevice(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := DeviceV2API{Registry: store, CommandPublisher: &v2CommandPublisherFake{}}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/PublishCommand", strings.NewReader(`{"deviceId":"nope","type":"set_led","parameters":{"on":true}}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type v2TelemetryReaderFake struct {
	payload   json.RawMessage
	timestamp string
	messageID string
	ok        bool
	err       error
}

func (f v2TelemetryReaderFake) LastTelemetry(context.Context, string) (json.RawMessage, string, string, bool, error) {
	return f.payload, f.timestamp, f.messageID, f.ok, f.err
}

func TestDeviceV2APIGetDeviceTelemetryStripsEnvelopeFields(t *testing.T) {
	reader := v2TelemetryReaderFake{ok: true, timestamp: "2026-10-02T12:00:00Z", messageID: "b4a5bb31-1710-4f7b-a043-1b6a292d04ad", payload: json.RawMessage(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-02T12:00:00Z","cpu_pct":12.5}`)}
	api := DeviceV2API{Telemetry: reader}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/GetDeviceTelemetry", strings.NewReader(`{"deviceId":"theia"}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Available bool                       `json:"available"`
		Fields    map[string]json.RawMessage `json:"fields"`
		Timestamp string                     `json:"timestamp"`
		MessageID string                     `json:"messageId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Available || body.Timestamp != "2026-10-02T12:00:00Z" || body.MessageID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" {
		t.Fatalf("body=%+v", body)
	}
	if _, has := body.Fields["message_id"]; has {
		t.Fatal("fields must not include message_id")
	}
	if _, has := body.Fields["timestamp"]; has {
		t.Fatal("fields must not include timestamp")
	}
	if string(body.Fields["cpu_pct"]) != "12.5" {
		t.Fatalf("fields.cpu_pct = %s", body.Fields["cpu_pct"])
	}
}

func TestDeviceV2APIGetDeviceTelemetryNotAvailable(t *testing.T) {
	api := DeviceV2API{Telemetry: v2TelemetryReaderFake{ok: false}}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/GetDeviceTelemetry", strings.NewReader(`{"deviceId":"theia"}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"available":false`) {
		t.Fatalf("body=%s", response.Body.String())
	}
}

func TestDeviceV2APIGetDeviceTelemetryReaderErrorIsBadGateway(t *testing.T) {
	api := DeviceV2API{Telemetry: v2TelemetryReaderFake{err: errors.New("internal API unreachable")}}
	request := httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/GetDeviceTelemetry", strings.NewReader(`{"deviceId":"theia"}`))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
