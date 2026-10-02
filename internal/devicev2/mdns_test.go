package devicev2

import (
	"net"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestAnnouncementFromMDNS(t *testing.T) {
	entry := &zeroconf.ServiceEntry{Port: 8080, AddrIPv4: []net.IP{net.ParseIP("192.0.2.9")}, Text: []string{"uid=abc", "model=esp32c3-led", "protocol=iot-device-v1", "firmware=2.0.0", "manifest_sha256=0123456789012345678901234567890123456789012345678901234567890123", "pairing=true", "status=unprovisioned", "path=/v1/device-info"}}
	a, err := announcementFromMDNS(entry)
	if err != nil {
		t.Fatal(err)
	}
	if a.Host != "192.0.2.9" || !a.Pairing || a.DeviceUID != "abc" || a.Path != "/v1/device-info" {
		t.Fatalf("announcement=%#v", a)
	}
}
