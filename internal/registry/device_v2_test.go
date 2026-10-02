package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const v2LED = `{"schema_version":2,"manifest_id":"esp32-c3-led","display_name":"ESP32-C3 LED","model":"esp32c3-led","protocol_version":1,"mqtt":{"publish":[{"channel":"state","retained":true,"schema":{}},{"channel":"event","events":[{"type":"led.state_changed","payload":{}}]}],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}]}]}}`

func TestV2ManifestAndDeviceRegistry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest, err := store.AcceptV2Manifest(ctx, v2LED, "lab-key")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.AcceptV2Manifest(ctx, v2LED, "other")
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != manifest.Revision || again.SHA256 != manifest.SHA256 {
		t.Fatal("manifest hash should be reused")
	}
	device, err := store.RegisterV2Device(ctx, V2Device{DeviceID: "led-sala", DeviceUID: "uid-001", ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: "public-key"})
	if err != nil {
		t.Fatal(err)
	}
	if device.DesiredState != "pending" {
		t.Fatalf("state=%s", device.DesiredState)
	}
	active, err := store.SetV2DeviceState(ctx, "led-sala", "active", "mqtt-confirmed")
	if err != nil || active.DesiredState != "active" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	resolved, ok, err := store.ResolveV2Manifest(ctx, "led-sala")
	if err != nil || !ok || resolved.ManifestID != "esp32-c3-led" {
		t.Fatalf("resolve=%#v %v %v", resolved, ok, err)
	}
	rule, err := store.CreateV2AutomationRule(ctx, V2AutomationRule{ID: "mirror-led", Enabled: true, SourceDeviceID: "led-sala", OutputChannel: "event", EventType: "led.state_changed", TargetDeviceID: "led-sala", CommandType: "set_led", ParametersJSON: `{"on":true}`})
	if err != nil || rule.EventType != "led.state_changed" {
		t.Fatalf("rule=%#v err=%v", rule, err)
	}
	if _, err := store.RegisterV2Device(ctx, V2Device{DeviceID: "led-sala", DeviceUID: "uid-001", ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: "public-key"}); !errors.Is(err, ErrV2DeviceAlreadyExists) {
		t.Fatalf("registering a live device again: err=%v, want ErrV2DeviceAlreadyExists", err)
	}
}

// TestRegisterV2DeviceReclaimsRemovedBinding guards against a real production
// incident: V2RegistrationCoordinator.Register's cleanup paths mark a failed
// registration's binding desired_state='removed' but never delete the row
// (nothing in this package does), so the next registration attempt for the
// same device hit RegisterV2Device's existence check and was rejected
// forever with ErrV2DeviceAlreadyExists - the device could never register.
func TestRegisterV2DeviceReclaimsRemovedBinding(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manifest, err := store.AcceptV2Manifest(ctx, v2LED, "lab-key")
	if err != nil {
		t.Fatal(err)
	}
	device := V2Device{DeviceID: "led-sala", DeviceUID: "uid-001", ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: "public-key"}
	if _, err := store.RegisterV2Device(ctx, device); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetV2DeviceState(ctx, "led-sala", "removed", "provisioning_failed"); err != nil {
		t.Fatal(err)
	}
	retried, err := store.RegisterV2Device(ctx, device)
	if err != nil {
		t.Fatalf("re-registering after a removed binding should succeed: %v", err)
	}
	if retried.DesiredState != "pending" {
		t.Fatalf("state=%s", retried.DesiredState)
	}
}
