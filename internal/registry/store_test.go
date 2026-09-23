package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

func TestSeedImportsLegacyPolicyOnlyOnce(t *testing.T) {
	store := openTestStore(t)
	device := testDevice("led-1", true)
	seeded, err := store.Seed(context.Background(), []config.Device{device}, nil)
	if err != nil || !seeded {
		t.Fatalf("Seed() = %t, %v", seeded, err)
	}
	seeded, err = store.Seed(context.Background(), []config.Device{testDevice("led-2", true)}, nil)
	if err != nil || seeded {
		t.Fatalf("second Seed() = %t, %v", seeded, err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || len(snapshot.Devices) != 1 || snapshot.Devices[0].ID != "led-1" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestAddDeviceIsIdempotentAndRevisioned(t *testing.T) {
	store := openTestStore(t)
	first, err := store.AddDevice(context.Background(), testDevice("led-1", true), "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || len(first.Devices) != 1 {
		t.Fatalf("first = %#v", first)
	}
	second, err := store.AddDevice(context.Background(), testDevice("led-1", true), "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 1 || len(second.Devices) != 1 {
		t.Fatalf("second = %#v", second)
	}
	_, err = store.AddDevice(context.Background(), testDevice("led-1", true), "request-2")
	if !errors.Is(err, ErrDeviceAlreadyExists) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestSetEnabledAndRemoveDeviceAdvanceRevision(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.AddDevice(context.Background(), testDevice("led-1", true), "add"); err != nil {
		t.Fatal(err)
	}
	device, err := store.SetDeviceEnabled(context.Background(), "led-1", false, "disable")
	if err != nil {
		t.Fatal(err)
	}
	if device.Enabled == nil || *device.Enabled {
		t.Fatalf("device = %#v", device)
	}
	if err := store.RemoveDevice(context.Background(), "led-1", "remove"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 3 || len(snapshot.Devices) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := store.RemoveDevice(context.Background(), "led-1", "remove-2"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestAddDeviceRejectsTopicOutsideDeviceNamespace(t *testing.T) {
	store := openTestStore(t)
	device := testDevice("led-1", true)
	device.Topics.Command = "devices/other/command"
	if _, err := store.AddDevice(context.Background(), device, "request-1"); err == nil {
		t.Fatal("AddDevice() error = nil")
	}
}

func TestAddAndRemoveRouteAdvanceRevision(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.AddDevice(context.Background(), config.Device{
		ID: "orangepi-monitor", Type: "linux", Enabled: boolPtr(true),
		Topics: config.Topics{Telemetry: "devices/orangepi-monitor/telemetry"},
	}, "source"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(context.Background(), testDevice("monitor", true), "destination"); err != nil {
		t.Fatal(err)
	}
	route := config.Route{
		ID: "orangepi-to-monitor", SourceTopic: "devices/orangepi-monitor/telemetry", DestinationTopic: "devices/monitor/command",
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_system_status"}, QoS: 1,
	}
	if err := store.AddRoute(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 3 || len(snapshot.Routes) != 1 || snapshot.Routes[0] != route {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := store.RemoveRoute(context.Background(), route.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 4 || len(snapshot.Routes) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestAddRouteRejectsDisabledOrUnknownEndpoints(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.AddDevice(context.Background(), testDevice("monitor", false), "destination"); err != nil {
		t.Fatal(err)
	}
	err := store.AddRoute(context.Background(), config.Route{
		ID: "invalid-route", SourceTopic: "devices/unknown/telemetry", DestinationTopic: "devices/monitor/command",
		Transform: config.RouteTransform{Type: "json_command", CommandType: "render_system_status"},
	})
	if err == nil {
		t.Fatal("AddRoute() error = nil")
	}
}

func TestRecordListAndResolveInconsistency(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if got, err := store.ListInconsistencies(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListInconsistencies() = %#v, %v, want empty", got, err)
	}

	if err := store.RecordInconsistency(ctx, "provision_cyd", "cyd-sala", "deliver CYD configuration failed", "revoke CYD credential also failed"); err != nil {
		t.Fatal(err)
	}

	list, err := store.ListInconsistencies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ListInconsistencies() = %#v, want 1 entry", list)
	}
	entry := list[0]
	if entry.Kind != "provision_cyd" || entry.DeviceID != "cyd-sala" {
		t.Errorf("entry = %#v", entry)
	}
	if entry.Cause == "" || entry.CompensationError == "" || entry.CreatedAt.IsZero() {
		t.Errorf("entry = %#v", entry)
	}

	if err := store.ResolveInconsistency(ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListInconsistencies(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListInconsistencies() after resolve = %#v, %v, want empty", got, err)
	}
}

func TestResolveInconsistencyRejectsUnknownOrAlreadyResolvedID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.ResolveInconsistency(ctx, "ghost"); !errors.Is(err, ErrInconsistencyNotFound) {
		t.Fatalf("ResolveInconsistency() error = %v, want ErrInconsistencyNotFound", err)
	}

	if err := store.RecordInconsistency(ctx, "remove_device", "led-1", "cause", "compensation"); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListInconsistencies(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("setup: ListInconsistencies() = %#v, %v", list, err)
	}
	if err := store.ResolveInconsistency(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveInconsistency(ctx, list[0].ID); !errors.Is(err, ErrInconsistencyNotFound) {
		t.Fatalf("ResolveInconsistency() on an already-resolved ID error = %v, want ErrInconsistencyNotFound", err)
	}
}

func TestDefaultDeviceManifestsAreSeededAndReadable(t *testing.T) {
	store := openTestStore(t)
	manifests, err := store.ListPublishedManifests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 3 {
		t.Fatalf("ListPublishedManifests() = %#v, want three seeds", manifests)
	}
	led, err := store.GetPublishedManifest(context.Background(), "esp32-c3-led")
	if err != nil {
		t.Fatal(err)
	}
	if led.Revision != 1 || led.DisplayName != "ESP32-C3 LED" || led.Document == "" || led.CreatedBy == "" || led.CreatedAt.IsZero() {
		t.Fatalf("GetPublishedManifest() = %#v", led)
	}
	if _, err := store.GetPublishedManifest(context.Background(), "missing"); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("GetPublishedManifest(missing) error = %v", err)
	}
}

func TestDeviceManifestBindingRequiresExistingPublishedRevisionAndDevice(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.BindDeviceManifest(ctx, "missing", "esp32-c3-led", 1); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("BindDeviceManifest(missing device) error = %v", err)
	}
	if _, err := store.AddDevice(ctx, testDevice("led-1", true), "add"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindDeviceManifest(ctx, "led-1", "esp32-c3-led", 2); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("BindDeviceManifest(missing revision) error = %v", err)
	}
	if err := store.BindDeviceManifest(ctx, "led-1", "esp32-c3-led", 1); err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testDevice(id string, enabled bool) config.Device {
	return config.Device{ID: id, Type: "esp32", Enabled: &enabled, Profile: "led.v1", Topics: config.Topics{Command: "devices/" + id + "/command"}}
}

func boolPtr(value bool) *bool { return &value }
