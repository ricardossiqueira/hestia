package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/cydprovision"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

type fakeCYDProvisioner struct {
	model        string
	inspectErr   error
	provisionErr error
	settings     cydprovision.Settings
	address      string
}

func (f *fakeCYDProvisioner) Inspect(context.Context, string) (cydprovision.DeviceInfo, error) {
	if f.inspectErr != nil {
		return cydprovision.DeviceInfo{}, f.inspectErr
	}
	model := f.model
	if model == "" {
		model = "cyd-monitor"
	}
	return cydprovision.DeviceInfo{
		Model: model, Status: "unprovisioned", ProtocolVersion: 1,
		DeviceUID: "00:11:22:33:44:55", FirmwareVersion: "test",
	}, nil
}

func (f *fakeCYDProvisioner) Provision(_ context.Context, address string, settings cydprovision.Settings) error {
	f.address = address
	f.settings = settings
	return f.provisionErr
}

type fakeCredentialStore struct {
	password string
	calls    []string
}

func (f *fakeCredentialStore) Provision(_ context.Context, id string, _ []string) (string, error) {
	f.calls = append(f.calls, "provision:"+id)
	return f.password, nil
}
func (f *fakeCredentialStore) Revoke(_ context.Context, id string) error {
	f.calls = append(f.calls, "revoke:"+id)
	return nil
}
func (f *fakeCredentialStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	if enabled {
		f.calls = append(f.calls, "enable:"+id)
	} else {
		f.calls = append(f.calls, "disable:"+id)
	}
	return nil
}

func TestProvisionDeviceByIPUsesPublishedManifest(t *testing.T) {
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	credentials := &fakeCredentialStore{password: "secret-that-must-not-enter-sqlite"}
	provisioner := &fakeCYDProvisioner{model: "esp32c3-led"}
	server, err := New(Config{
		Registry: store, Credentials: credentials,
		ProvisioningClient: func(model string) cydprovision.Client {
			if model != "esp32c3-led" {
				t.Fatalf("requested model = %q", model)
			}
			return provisioner
		},
		DeviceBrokerHost: "192.168.15.195", DeviceBrokerPort: 1884,
	})
	if err != nil {
		t.Fatal(err)
	}

	device, address, err := server.ProvisionDeviceByIP(context.Background(), "led-sala", "esp32-c3-led", "192.168.15.43")
	if err != nil {
		t.Fatal(err)
	}
	if address != "192.168.15.43" || device.Type != "esp32c3-led" || device.Enabled == nil || !*device.Enabled {
		t.Fatalf("result = %#v, %q", device, address)
	}
	if device.Topics.State != "devices/led-sala/state" || device.Topics.Command != "devices/led-sala/command" {
		t.Fatalf("device topics = %#v", device.Topics)
	}
	if provisioner.settings.Password != credentials.password || provisioner.settings.Username != "led-sala" {
		t.Fatalf("delivered settings = %#v", provisioner.settings)
	}
	if got := credentials.calls; len(got) != 3 || got[0] != "provision:led-sala" || got[1] != "disable:led-sala" || got[2] != "enable:led-sala" {
		t.Fatalf("credential calls = %#v", got)
	}
}

func TestProvisionDeviceByIPRejectsIncompatibleFirmwareBeforeRegistryWrite(t *testing.T) {
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server, err := New(Config{
		Registry: store, Credentials: &fakeCredentialStore{password: "secret"},
		ProvisioningClient: func(string) cydprovision.Client {
			return &fakeCYDProvisioner{model: "esp32c3-led", inspectErr: cydprovision.ErrUnexpectedDevice}
		},
		DeviceBrokerHost: "192.168.15.195", DeviceBrokerPort: 1884,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = server.ProvisionDeviceByIP(context.Background(), "led-sala", "esp32-c3-led", "192.168.15.43")
	if !errors.Is(err, ErrDeviceNotProvisionable) {
		t.Fatalf("ProvisionDeviceByIP() error = %v", err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Devices) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRegisterExistingDeviceAddsMonitorWithoutTouchingCredential(t *testing.T) {
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	credentials := &fakeCredentialStore{password: "must-not-be-read"}
	server, err := New(Config{Registry: store, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	device, err := server.RegisterExistingDevice(context.Background(), "orangepi-monitor", "orangepi_monitor.v1")
	if err != nil {
		t.Fatal(err)
	}
	if device.Topics.Telemetry != "devices/orangepi-monitor/telemetry" || device.Enabled == nil || !*device.Enabled {
		t.Fatalf("device = %#v", device)
	}
	if len(credentials.calls) != 0 {
		t.Fatalf("credential store calls = %#v, want none", credentials.calls)
	}
}
