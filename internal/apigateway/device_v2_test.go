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

func TestDeviceV2APIRegistersReadyDiscovery(t *testing.T) {
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
	announcement := devicev2.Announcement{DeviceUID: "test-uid", Host: "192.0.2.10", Port: 8080, Model: "test-led", Protocol: "iot-device-v1", Firmware: "2.0.0", ManifestSHA256: hash, Pairing: false}
	if err := inbox.Observe(announcement); err != nil {
		t.Fatal(err)
	}
	inbox.MarkInspected("test-uid", devicev2.DeviceInfo{DeviceUID: "test-uid", Model: "test-led", FirmwareVersion: "2.0.0", Manifest: json.RawMessage(canonical), ManifestSHA256: hash, IdentityPublicKey: "public", PairingRequired: false}, nil)
	api := DeviceV2API{Inbox: inbox, Registry: store}
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
	get := httptest.NewRecorder()
	api.ServeHTTP(get, httptest.NewRequest(http.MethodPost, "/iot.gateway.api.v2.DevicePlatformService/GetDevice", strings.NewReader(`{"deviceId":"led-sala"}`)))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"deviceId":"led-sala"`) {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
}
