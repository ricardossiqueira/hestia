package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

const v2LEDManifest = `{"schema_version":2,"manifest_id":"esp32-c3-led","display_name":"ESP32-C3 LED","model":"esp32c3-led","protocol_version":1,"mqtt":{"publish":[{"channel":"state","retained":true,"schema":{}},{"channel":"event","events":[{"type":"led.state_changed","payload":{}}]}],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}]}]}}`

type v2IssuerFake struct {
	calls []string
	fail  string
}

func (f *v2IssuerFake) ProvisionV2(_ context.Context, id string, _ devicev2.Manifest) (string, error) {
	f.calls = append(f.calls, "provision:"+id)
	if f.fail == "provision" {
		return "", errors.New("provision failed")
	}
	return "generated-secret", nil
}
func (f *v2IssuerFake) SetEnabled(_ context.Context, id string, enabled bool) error {
	f.calls = append(f.calls, "enabled:"+id+":"+map[bool]string{true: "true", false: "false"}[enabled])
	if f.fail == "enable" && enabled {
		return errors.New("enable failed")
	}
	return nil
}
func (f *v2IssuerFake) Revoke(_ context.Context, id string) error {
	f.calls = append(f.calls, "revoke:"+id)
	return nil
}

type v2ProvisionerFake struct {
	settings devicev2.ProvisionSettings
	err      error
}

func (f *v2ProvisionerFake) PairAndProvision(_ context.Context, _ devicev2.Announcement, _ devicev2.DeviceInfo, settings devicev2.ProvisionSettings) error {
	f.settings = settings
	return f.err
}

func TestV2RegistrationCoordinatorActivatesOnlyAfterSecureProvisioning(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest, canonical, hash, err := devicev2.Parse(v2LEDManifest)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &v2IssuerFake{}
	provisioner := &v2ProvisionerFake{}
	coordinator := V2RegistrationCoordinator{Registry: store, Credentials: issuer, Provisioner: provisioner, MQTTBrokerHost: "mqtt.local", MQTTBrokerPort: 8883}
	entry := devicev2.DiscoveryEntry{Announcement: devicev2.Announcement{DeviceUID: "uid-1", Model: manifest.Model, ManifestSHA256: hash}, State: devicev2.PairingRequired, Info: &devicev2.DeviceInfo{DeviceUID: "uid-1", Model: manifest.Model, FirmwareVersion: "2.0.0", Manifest: []byte(canonical), ManifestSHA256: hash, IdentityPublicKey: "key", PairingRequired: true}}
	device, err := coordinator.Register(ctx, entry, "led-sala")
	if err != nil {
		t.Fatal(err)
	}
	if device.DesiredState != "active" || device.ActiveState != "active" {
		t.Fatalf("device=%#v", device)
	}
	if provisioner.settings.MQTTPassword != "generated-secret" || provisioner.settings.DeviceID != "led-sala" {
		t.Fatalf("settings=%#v", provisioner.settings)
	}
	want := []string{"provision:led-sala", "enabled:led-sala:false", "enabled:led-sala:true"}
	if len(issuer.calls) != len(want) {
		t.Fatalf("calls=%v", issuer.calls)
	}
	for i := range want {
		if issuer.calls[i] != want[i] {
			t.Fatalf("calls=%v want=%v", issuer.calls, want)
		}
	}
}

func TestV2RegistrationCoordinatorRevokesOnDeliveryFailure(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest, canonical, hash, err := devicev2.Parse(v2LEDManifest)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &v2IssuerFake{}
	provisioner := &v2ProvisionerFake{err: errors.New("pairing rejected")}
	coordinator := V2RegistrationCoordinator{Registry: store, Credentials: issuer, Provisioner: provisioner, MQTTBrokerHost: "mqtt.local", MQTTBrokerPort: 8883}
	entry := devicev2.DiscoveryEntry{Announcement: devicev2.Announcement{DeviceUID: "uid-2", Model: manifest.Model, ManifestSHA256: hash}, State: devicev2.PairingRequired, Info: &devicev2.DeviceInfo{DeviceUID: "uid-2", Model: manifest.Model, FirmwareVersion: "2.0.0", Manifest: []byte(canonical), ManifestSHA256: hash, IdentityPublicKey: "key", PairingRequired: true}}
	if _, err := coordinator.Register(ctx, entry, "led-falha"); err == nil {
		t.Fatal("Register() error = nil")
	}
	device, err := store.GetV2Device(ctx, "led-falha")
	if err != nil {
		t.Fatal(err)
	}
	if device.DesiredState != "removed" || device.ActiveState != "provisioning_failed" {
		t.Fatalf("device=%#v", device)
	}
	if got := issuer.calls[len(issuer.calls)-1]; got != "revoke:led-falha" {
		t.Fatalf("calls=%v", issuer.calls)
	}
}
