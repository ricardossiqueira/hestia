package apigateway

import (
	"context"
	"encoding/json"
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
