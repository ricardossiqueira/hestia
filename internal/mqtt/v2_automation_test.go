package mqtt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

const v2RuntimeLED = `{"schema_version":2,"manifest_id":"runtime-led","display_name":"Runtime LED","model":"runtime-led","protocol_version":1,"mqtt":{"publish":[{"channel":"event","events":[{"type":"led.state_changed","payload":{"on":{"type":"boolean","required":true}}}]},{"channel":"state","retained":true,"schema":{"on":{"type":"boolean","required":true}}}],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}]}]}}`
const v2RuntimeCYD = `{"schema_version":2,"manifest_id":"runtime-cyd","display_name":"Runtime CYD","model":"runtime-cyd","protocol_version":1,"mqtt":{"publish":[],"subscribe":[{"channel":"command","commands":[{"type":"render_system_status","parameters":{"timestamp":{"type":"string","required":true}}}]}]}}`

func setupV2Runtime(t *testing.T, sourceOnly bool) (*registry.Store, *fakeClient, *Gateway) {
	t.Helper()
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	led, err := store.AcceptV2Manifest(ctx, v2RuntimeLED, "test")
	if err != nil {
		t.Fatal(err)
	}
	registerV2RuntimeDevice(t, store, "led-source", "uid-source", led, "key-source")
	if !sourceOnly {
		cyd, err := store.AcceptV2Manifest(ctx, v2RuntimeCYD, "test")
		if err != nil {
			t.Fatal(err)
		}
		registerV2RuntimeDevice(t, store, "cyd-target", "uid-target", cyd, "key-target")
	}
	client := &fakeClient{}
	gateway, err := New(config.Config{}, client, &recordingLogger{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.EnableV2Runtime(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close(); _ = store.Close() })
	return store, client, gateway
}

func registerV2RuntimeDevice(t *testing.T, store *registry.Store, id, uid string, manifest registry.V2Manifest, key string) {
	t.Helper()
	ctx := context.Background()
	device, err := store.RegisterV2Device(ctx, registry.V2Device{DeviceID: id, DeviceUID: uid, ManifestID: manifest.ManifestID, ManifestRevision: manifest.Revision, ManifestSHA256: manifest.SHA256, FirmwareVersion: "2.0.0", IdentityPublicKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetV2DeviceState(ctx, device.DeviceID, "active", "mqtt-confirmed"); err != nil {
		t.Fatal(err)
	}
}

func TestV2RuntimeExecutesPersistedRuleForDeclaredEventAndCommandOnlyTarget(t *testing.T) {
	store, client, _ := setupV2Runtime(t, false)
	_, err := store.CreateV2AutomationRule(context.Background(), registry.V2AutomationRule{ID: "event-to-cyd", Enabled: true, SourceDeviceID: "led-source", OutputChannel: "event", EventType: "led.state_changed", TargetDeviceID: "cyd-target", CommandType: "render_system_status", ParametersJSON: `{"timestamp":"2026-10-01T12:00:00Z"}`})
	if err != nil {
		t.Fatal(err)
	}
	client.deliver("devices/led-source/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","type":"led.state_changed","on":true}`))
	if len(client.published) != 1 {
		t.Fatalf("published=%d, want 1", len(client.published))
	}
	got := client.published[0]
	if got.topic != "devices/cyd-target/command" || got.qos != qosAtLeastOnce || got.retain {
		t.Fatalf("publication=%#v", got)
	}
	var command struct {
		Type       string            `json:"type"`
		Parameters map[string]string `json:"parameters"`
	}
	if err := json.Unmarshal(got.payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != "render_system_status" || command.Parameters["timestamp"] == "" {
		t.Fatalf("command=%s", got.payload)
	}
	// Same delivery has the same causal ID: its execution reservation makes it
	// a no-op even though the broker uses at-least-once delivery.
	client.deliver("devices/led-source/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","type":"led.state_changed","on":true}`))
	if len(client.published) != 1 {
		t.Fatalf("duplicate published=%d, want 1", len(client.published))
	}
}

func TestV2RuntimeRejectsUndeclaredOutputAndEventReplay(t *testing.T) {
	_, client, gateway := setupV2Runtime(t, true)
	client.deliver("devices/led-source/event", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","type":"led.state_changed","on":true,"unexpected":1}`))
	client.deliverRetained("devices/led-source/event", []byte(`{"message_id":"c4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","type":"led.state_changed","on":true}`))
	if gateway.Snapshot().RejectedMessages != 2 {
		t.Fatalf("rejected=%d, want 2", gateway.Snapshot().RejectedMessages)
	}
}

func TestV2RuntimeIgnoresRetainedStateAndAppliesConditionAndRateLimit(t *testing.T) {
	store, client, _ := setupV2Runtime(t, true)
	_, err := store.CreateV2AutomationRule(context.Background(), registry.V2AutomationRule{ID: "state-to-led", Enabled: true, SourceDeviceID: "led-source", OutputChannel: "state", IgnoreRetained: true, ConditionJSON: `{"==":[{"var":"on"},true]}`, TargetDeviceID: "led-source", CommandType: "set_led", ParametersJSON: `{"on":true}`})
	if err != nil {
		t.Fatal(err)
	}
	retained := []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","on":true}`)
	client.deliverRetained("devices/led-source/state", retained)
	if len(client.published) != 0 {
		t.Fatal("retained state fired rule")
	}
	// One false condition plus six unique true messages proves both condition
	// evaluation and the five-per-ten-seconds guard.
	client.deliver("devices/led-source/state", []byte(`{"message_id":"c4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-01T12:00:00Z","on":false}`))
	for _, id := range []string{"d4a5bb31-1710-4f7b-a043-1b6a292d04ad", "e4a5bb31-1710-4f7b-a043-1b6a292d04ad", "f4a5bb31-1710-4f7b-a043-1b6a292d04ad", "14a5bb31-1710-4f7b-a043-1b6a292d04ad", "24a5bb31-1710-4f7b-a043-1b6a292d04ad", "34a5bb31-1710-4f7b-a043-1b6a292d04ad"} {
		client.deliver("devices/led-source/state", []byte(`{"message_id":"`+id+`","timestamp":"2026-10-01T12:00:00Z","on":true}`))
	}
	if len(client.published) != maxRuleFiringsPerRule {
		t.Fatalf("published=%d, want rate limit %d", len(client.published), maxRuleFiringsPerRule)
	}
}
