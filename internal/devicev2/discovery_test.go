package devicev2

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestHTTPInspectorAndInbox(t *testing.T) {
	_, canonical, hash, err := Parse(ledManifest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var manifest any
		if err := json.Unmarshal([]byte(canonical), &manifest); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": "iot-device-v1", "device_uid": "uid-1", "model": "esp32c3-led", "firmware_version": "2.0.0", "manifest": manifest, "manifest_sha256": hash, "identity_public_key": "pk", "identity_signature": "sig", "pairing_required": true, "provisioning_state": "unregistered"})
	}))
	defer server.Close()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	a := Announcement{DeviceUID: "uid-1", Host: host, Port: uint16(port), Model: "esp32c3-led", Protocol: "iot-device-v1", ManifestSHA256: hash}
	inspector := HTTPInspector{Client: server.Client()}
	info, parsed, gotCanonical, err := inspector.Inspect(t.Context(), a)
	if err != nil || info.DeviceUID != a.DeviceUID || parsed.ManifestID != "esp32-c3-led" || gotCanonical != canonical {
		t.Fatalf("inspect=%#v %#v %q err=%v", info, parsed, gotCanonical, err)
	}
	inbox := NewInbox(time.Second)
	if err := inbox.Observe(a); err != nil {
		t.Fatal(err)
	}
	inbox.MarkInspected("uid-1", DeviceInfo{PairingRequired: true}, nil)
	if got := inbox.List()[0].State; got != PairingRequired {
		t.Fatalf("state=%s", got)
	}
}

func TestDeviceURLFormatsIPv6Authority(t *testing.T) {
	got := deviceURL(Announcement{Host: "fe80::1234", Port: 8080}, "/v1/device-info")
	const want = "http://[fe80::1234]:8080/v1/device-info"
	if got != want {
		t.Fatalf("deviceURL() = %q, want %q", got, want)
	}
}
