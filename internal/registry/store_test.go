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
