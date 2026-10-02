package mqtt

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

const v2RuntimeTelemetryDevice = `{"schema_version":2,"manifest_id":"runtime-telemetry","display_name":"Runtime Telemetry","model":"runtime-telemetry","protocol_version":1,"mqtt":{"publish":[{"channel":"telemetry","schema":{"cpu_pct":{"type":"number","required":true}}}],"subscribe":[]}}`

func TestLastTelemetryRecordsAndReplacesAcceptedMessages(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.AcceptV2Manifest(ctx, v2RuntimeTelemetryDevice, "test")
	if err != nil {
		t.Fatal(err)
	}
	registerV2RuntimeDevice(t, store, "theia", "uid-theia", manifest, "key-theia")
	client := &fakeClient{}
	gateway, err := New(client, &recordingLogger{})
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

	if _, ok := gateway.LastTelemetry("theia"); ok {
		t.Fatal("LastTelemetry before any message = ok, want no entry yet")
	}

	client.deliver("devices/theia/telemetry", []byte(`{"message_id":"b4a5bb31-1710-4f7b-a043-1b6a292d04ad","timestamp":"2026-10-02T12:00:00Z","cpu_pct":12.5}`))
	message, ok := gateway.LastTelemetry("theia")
	if !ok {
		t.Fatal("LastTelemetry after one message = no entry, want ok")
	}
	if message.DeviceID != "theia" || message.MessageID != "b4a5bb31-1710-4f7b-a043-1b6a292d04ad" {
		t.Fatalf("message=%#v", message)
	}

	// A second message replaces the first - no history is kept.
	client.deliver("devices/theia/telemetry", []byte(`{"message_id":"c5b6cc42-2821-5f8c-b154-2c7b3a3e15be","timestamp":"2026-10-02T12:00:02Z","cpu_pct":13.1}`))
	message, ok = gateway.LastTelemetry("theia")
	if !ok || message.MessageID != "c5b6cc42-2821-5f8c-b154-2c7b3a3e15be" {
		t.Fatalf("message after second delivery=%#v, ok=%v", message, ok)
	}

	// A rejected message (schema violation) must not overwrite the cache.
	client.deliver("devices/theia/telemetry", []byte(`{"message_id":"d6c7dd53-3932-6f9d-c265-3d8c4b4f26cf","timestamp":"2026-10-02T12:00:04Z","cpu_pct":"not-a-number"}`))
	message, ok = gateway.LastTelemetry("theia")
	if !ok || message.MessageID != "c5b6cc42-2821-5f8c-b154-2c7b3a3e15be" {
		t.Fatalf("message after rejected delivery=%#v, ok=%v", message, ok)
	}

	if _, ok := gateway.LastTelemetry("unknown-device"); ok {
		t.Fatal("LastTelemetry for unknown device = ok, want no entry")
	}
}
