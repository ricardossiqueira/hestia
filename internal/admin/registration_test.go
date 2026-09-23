package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// writeFakeProvisionScript writes a .bat standing in for
// deploy/mosquitto-provision-device.sh: `<id> <topics...>` (provision mode)
// prints the script's real password trailer so Provision's regex parses
// it; `--remove <id>` (deprovision mode) exits 0 unless failDeprovision is
// set, in which case it exits 1. Every invocation appends one line to
// callLog, so a test can assert exactly what ran (and in rollback tests,
// that Deprovision really was called).
func writeFakeProvisionScript(t *testing.T, callLog string, failDeprovision bool) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		scriptPath := filepath.Join(t.TempDir(), "provision.sh")
		deprovisionExit := "0"
		if failDeprovision {
			deprovisionExit = "1"
		}
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"--remove\" ]; then\n" +
			"  echo \"REMOVE $2\" >> \"" + callLog + "\"\n" +
			"  exit " + deprovisionExit + "\n" +
			"fi\n" +
			"echo \"PROVISION $1\" >> \"" + callLog + "\"\n" +
			"echo '  #define MQTT_PASSWORD \"fake-password-123\"'\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return scriptPath
	}
	scriptPath := filepath.Join(t.TempDir(), "provision.bat")
	deprovisionBody := `echo REMOVE %2>>"` + callLog + `"` + "\r\n\texit /b 0"
	if failDeprovision {
		deprovisionBody = `echo REMOVE %2>>"` + callLog + `"` + "\r\n\texit /b 1"
	}
	script := "@echo off\r\n" +
		`if "%1"=="--remove" (` + "\r\n\t" + deprovisionBody + "\r\n)\r\n" +
		`echo PROVISION %1>>"` + callLog + `"` + "\r\n" +
		`echo   #define MQTT_PASSWORD "fake-password-123"` + "\r\n" +
		"exit /b 0\r\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

func readCallLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRegisterDevice_Success(t *testing.T) {
	path := writeFixture(t, baseYAML)
	callLog := filepath.Join(t.TempDir(), "calls.log")
	script := writeFakeProvisionScript(t, callLog, false)

	device, password, err := RegisterDevice(context.Background(), path, script, "led-1", "esp32_led.v1")
	// RestartGateway shells out to the real `systemctl`, which does not
	// exist on this dev machine (Windows) - accept ONLY that specific
	// failure here; anything else (provisioning, the YAML write) must
	// still have fully succeeded, which the assertions below confirm.
	if err != nil && !strings.Contains(err.Error(), "failed to restart") {
		t.Fatalf("RegisterDevice() error = %v", err)
	}
	if password != "fake-password-123" {
		t.Errorf("password = %q", password)
	}
	if device.ID != "led-1" || device.Type != "esp32" || device.Profile != "led.v1" {
		t.Errorf("device = %#v", device)
	}
	if !strings.Contains(readCallLog(t, callLog), "PROVISION led-1") {
		t.Error("provisioning script was not invoked in provision mode")
	}
	if strings.Contains(readCallLog(t, callLog), "REMOVE") {
		t.Error("deprovision must not run on a successful registration")
	}
}

func TestServerRegistryPathDoesNotRestartGateway(t *testing.T) {
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	callLog := filepath.Join(t.TempDir(), "calls.log")
	server, err := New(Config{Registry: store, ProvisionScript: writeFakeProvisionScript(t, callLog, false)})
	if err != nil {
		t.Fatal(err)
	}
	device, password, err := server.ProvisionDevice(context.Background(), "led-1", "esp32_led.v1")
	if err != nil {
		t.Fatal(err)
	}
	if password != "fake-password-123" || device.ID != "led-1" {
		t.Fatalf("provision = %#v, %q", device, password)
	}
	if _, err := server.SetDeviceEnabled(context.Background(), "led-1", false); err != nil {
		t.Fatal(err)
	}
	if err := server.RemoveDevice(context.Background(), "led-1"); err != nil {
		t.Fatal(err)
	}
	if got := readCallLog(t, callLog); !strings.Contains(got, "PROVISION led-1") || !strings.Contains(got, "REMOVE led-1") {
		t.Fatalf("calls = %q", got)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Devices) != 0 {
		t.Fatalf("registry devices = %#v", snapshot.Devices)
	}
}

func TestRegisterDevice_UnknownTemplate(t *testing.T) {
	path := writeFixture(t, baseYAML)
	script := writeFakeProvisionScript(t, filepath.Join(t.TempDir(), "calls.log"), false)

	_, _, err := RegisterDevice(context.Background(), path, script, "led-1", "no-such-template")
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("RegisterDevice() error = %v, want ErrUnknownTemplate", err)
	}
}

func TestRegisterDevice_InvalidID(t *testing.T) {
	path := writeFixture(t, baseYAML)
	script := writeFakeProvisionScript(t, filepath.Join(t.TempDir(), "calls.log"), false)

	_, _, err := RegisterDevice(context.Background(), path, script, "Not Valid!", "esp32_led.v1")
	if !errors.Is(err, ErrInvalidDeviceID) {
		t.Fatalf("RegisterDevice() error = %v, want ErrInvalidDeviceID", err)
	}
}

func TestRegisterDevice_AlreadyExists(t *testing.T) {
	path := writeFixture(t, baseYAML)
	script := writeFakeProvisionScript(t, filepath.Join(t.TempDir(), "calls.log"), false)

	_, _, err := RegisterDevice(context.Background(), path, script, "esp32-sala", "esp32_led.v1")
	if !errors.Is(err, ErrDeviceAlreadyExists) {
		t.Fatalf("RegisterDevice() error = %v, want ErrDeviceAlreadyExists", err)
	}
}

func TestRegisterDevice_RollsBackCredentialWhenYAMLWriteFails(t *testing.T) {
	path := writeFixture(t, baseYAML)
	// Read-only main file: writeValidated writes path+".bak" first (that
	// succeeds, it's a different, writable filename), then fails writing
	// path itself - forcing AddDeviceFromTemplate to fail AFTER Provision
	// already succeeded, exactly the window RegisterDevice must roll back.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	callLog := filepath.Join(t.TempDir(), "calls.log")
	script := writeFakeProvisionScript(t, callLog, false)

	_, _, err := RegisterDevice(context.Background(), path, script, "led-1", "esp32_led.v1")
	if err == nil {
		t.Fatal("RegisterDevice() error = nil, want the forced YAML write failure")
	}
	log := readCallLog(t, callLog)
	if !strings.Contains(log, "PROVISION led-1") {
		t.Fatalf("provisioning script was not invoked: %s", log)
	}
	if !strings.Contains(log, "REMOVE led-1") {
		t.Fatalf("rollback (Deprovision) was NOT called after the YAML write failed: %s", log)
	}
}

func TestDeregisterDevice_Success(t *testing.T) {
	path := writeFixture(t, baseYAML)
	// A second device keeps the config valid after esp32-sala is removed -
	// config.Validate() rejects an empty devices list, same reason
	// TestRemoveDeviceDeletesOnlyTheMatch does this in devices_test.go.
	if err := AddDevice(path, "esp32c3-led", []string{"command"}); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(t.TempDir(), "calls.log")
	script := writeFakeProvisionScript(t, callLog, false)

	err := DeregisterDevice(context.Background(), path, script, "esp32-sala")
	// Same systemctl-not-on-this-dev-machine tolerance as
	// TestRegisterDevice_Success above.
	if err != nil && !strings.Contains(err.Error(), "failed to restart") {
		t.Fatalf("DeregisterDevice() error = %v", err)
	}
	if !strings.Contains(readCallLog(t, callLog), "REMOVE esp32-sala") {
		t.Error("deprovision script was not invoked")
	}
}

func TestDeregisterDevice_UnknownID(t *testing.T) {
	path := writeFixture(t, baseYAML)
	script := writeFakeProvisionScript(t, filepath.Join(t.TempDir(), "calls.log"), false)

	err := DeregisterDevice(context.Background(), path, script, "no-such-device")
	if err == nil || !strings.Contains(err.Error(), "removing") {
		t.Fatalf("DeregisterDevice() error = %v, want a gateway.yaml removal error", err)
	}
}

func TestDeregisterDevice_DeprovisionFails_NoRollback(t *testing.T) {
	path := writeFixture(t, baseYAML)
	if err := AddDevice(path, "esp32c3-led", []string{"command"}); err != nil {
		t.Fatal(err)
	}
	script := writeFakeProvisionScript(t, filepath.Join(t.TempDir(), "calls.log"), true)

	err := DeregisterDevice(context.Background(), path, script, "esp32-sala")
	if err == nil || !strings.Contains(err.Error(), "could not be revoked") {
		t.Fatalf("DeregisterDevice() error = %v, want a deprovision-failed error", err)
	}
	// No rollback: the device must stay removed from gateway.yaml even
	// though revoking its credential failed - matches handleRemoveDevice's
	// long-standing posture (error + manual instruction, not auto-restore).
	devices, err := ListDevices(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].ID != "esp32c3-led" {
		t.Errorf("devices = %#v, want only esp32c3-led left (esp32-sala stays removed)", devices)
	}
}
