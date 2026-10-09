package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestV2AutomationRuleMutationsPreserveExecutionHistory(t *testing.T) {
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
	if _, err := store.RegisterV2Device(ctx, V2Device{DeviceID: "led-sala", DeviceUID: "uid-001", ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: "public-key"}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	rule := V2AutomationRule{ID: "mirror-led", Enabled: true, SourceDeviceID: "led-sala", OutputChannel: "event", EventType: "led.state_changed", TargetDeviceID: "led-sala", CommandType: "set_led", ParametersJSON: `{"on":true}`}
	created, err := store.CreateV2AutomationRule(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	if created.UpdatedAt.IsZero() {
		t.Fatal("create must assign updatedAt")
	}

	updated := rule
	updated.Enabled = false
	updated.ConditionJSON = `{"==":[{"var":"on"},true]}`
	updated.ParametersJSON = `{"on":false}`
	updated.UpdatedAt = time.Time{}
	got, err := store.UpdateV2AutomationRule(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rule.ID || got.Enabled || got.ConditionJSON != updated.ConditionJSON || got.ParametersJSON != updated.ParametersJSON {
		t.Fatalf("updated rule=%+v", got)
	}
	if got.UpdatedAt.IsZero() || !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("updatedAt=%s, want server time %s", got.UpdatedAt, created.UpdatedAt)
	}

	enabled, err := store.SetV2AutomationRuleEnabled(ctx, rule.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.ConditionJSON != updated.ConditionJSON || enabled.ParametersJSON != updated.ParametersJSON {
		t.Fatalf("toggle changed unrelated fields: %+v", enabled)
	}

	reserved, err := store.ReserveV2AutomationExecution(ctx, rule.ID, "message-1", "command-1", "led-sala")
	if err != nil || !reserved {
		t.Fatalf("reserve=%v err=%v", reserved, err)
	}
	if err := store.CompleteV2AutomationExecution(ctx, rule.ID, "message-1", true); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveV2AutomationRule(ctx, rule.ID); err != nil {
		t.Fatal(err)
	}
	if rules, err := store.ListV2AutomationRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("visible rules=%+v err=%v", rules, err)
	}
	if count, err := store.CountV2PublishedAutomationExecutionsSince(ctx, rule.ID, time.Time{}); err != nil || count != 1 {
		t.Fatalf("published execution count=%d err=%v", count, err)
	}
	if _, err := store.CreateV2AutomationRule(ctx, rule); !errors.Is(err, ErrV2AutomationRuleExists) {
		t.Fatalf("reusing removed ID err=%v, want ErrV2AutomationRuleExists", err)
	}
	if _, err := store.UpdateV2AutomationRule(ctx, rule); !errors.Is(err, ErrV2AutomationRuleNotFound) {
		t.Fatalf("updating removed rule err=%v, want ErrV2AutomationRuleNotFound", err)
	}
	if _, err := store.SetV2AutomationRuleEnabled(ctx, rule.ID, false); !errors.Is(err, ErrV2AutomationRuleNotFound) {
		t.Fatalf("toggling removed rule err=%v, want ErrV2AutomationRuleNotFound", err)
	}
	if err := store.RemoveV2AutomationRule(ctx, rule.ID); !errors.Is(err, ErrV2AutomationRuleNotFound) {
		t.Fatalf("removing rule twice err=%v, want ErrV2AutomationRuleNotFound", err)
	}
}

func TestV2AutomationRuleUpdateValidatesAgainstManifest(t *testing.T) {
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
	if _, err := store.RegisterV2Device(ctx, V2Device{DeviceID: "led-sala", DeviceUID: "uid-001", ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: "public-key"}); err != nil {
		t.Fatal(err)
	}
	rule := V2AutomationRule{ID: "mirror-led", Enabled: true, SourceDeviceID: "led-sala", OutputChannel: "event", EventType: "led.state_changed", TargetDeviceID: "led-sala", CommandType: "set_led", ParametersJSON: `{"on":true}`}
	if _, err := store.CreateV2AutomationRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	invalid := rule
	invalid.ParametersJSON = `{"on":"yes"}`
	if _, err := store.UpdateV2AutomationRule(ctx, invalid); !errors.Is(err, ErrV2AutomationRuleInvalid) {
		t.Fatalf("invalid update err=%v, want ErrV2AutomationRuleInvalid", err)
	}
	current, err := store.ListV2AutomationRules(ctx)
	if err != nil || len(current) != 1 || current[0].ParametersJSON != `{"on":true}` {
		t.Fatalf("invalid update changed persisted rule: %+v err=%v", current, err)
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
