package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// TestRemoveDeviceRecordsInconsistencyWhenRegistryRemovalFails exercises one
// representative recordInconsistency call site end to end (registry write
// really happens, on the live store - see recordInconsistency's doc comment
// on why it always uses context.Background()). The other call sites in
// server.go follow the identical pattern; this is not re-verified for each
// one.
//
// Registry.RemoveDevice is made to fail without closing the store (which
// would also break recordInconsistency's own write) by canceling ctx before
// the call: fakeCredentialStore.Revoke ignores its context entirely and
// always succeeds, so this reproduces exactly the case ADR-014 describes -
// credential already revoked, registry removal fails - without needing a
// real disk/DB fault.
func TestRemoveDeviceRecordsInconsistencyWhenRegistryRemovalFails(t *testing.T) {
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	credentials := &fakeCredentialStore{}
	server, err := New(Config{Registry: store, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := store.AddDevice(context.Background(), config.Device{
		ID: "led-1", Type: "esp32", Enabled: &enabled, Topics: config.Topics{Command: "devices/led-1/command"},
	}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	if got, err := server.ListInconsistencies(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("setup: ListInconsistencies() = %#v, %v, want empty", got, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = server.RemoveDevice(canceled, "led-1")
	if err == nil {
		t.Fatal("RemoveDevice() error = nil, want the canceled-context registry failure")
	}

	// recordInconsistency runs on context.Background(), not the canceled
	// ctx above, so this read (on a fresh, non-canceled context) must see it.
	list, err := server.ListInconsistencies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ListInconsistencies() = %#v, want 1 entry", list)
	}
	entry := list[0]
	if entry.Kind != "remove_device" || entry.DeviceID != "led-1" {
		t.Errorf("entry = %#v", entry)
	}
	if entry.Cause == "" || entry.CompensationError == "" {
		t.Errorf("entry = %#v, want non-empty Cause/CompensationError", entry)
	}
	if time.Since(entry.CreatedAt) > time.Minute {
		t.Errorf("CreatedAt = %v, looks stale", entry.CreatedAt)
	}

	if err := server.ResolveInconsistency(context.Background(), entry.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := server.ListInconsistencies(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("ListInconsistencies() after resolve = %#v, %v, want empty", got, err)
	}
}
