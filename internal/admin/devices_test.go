package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

const baseYAML = `# comment that must survive a rewrite
gateway:
  id: orangepi-lab-01
  timezone: America/Sao_Paulo
mqtt:
  url: mqtt://127.0.0.1:1883
  client_id: iot-gateway-orangepi-lab-01
  username_env: MQTT_USER
  password_env: MQTT_PASSWORD
storage:
  sqlite_path: /var/lib/iot-gateway/gateway.db
  max_outbox_messages: 100
  max_outbox_bytes: 1048576
  max_outbox_age: 24h
devices:
  - id: esp32-sala
    type: esp32
    enabled: true
    topics:
      telemetry: devices/esp32-sala/telemetry
      state: devices/esp32-sala/state
      event: devices/esp32-sala/event
      command: devices/esp32-sala/command
      command_result: devices/esp32-sala/command-result
    forwarding:
      telemetry_to_vps: true
      state_to_vps: true
      events_to_vps: true
      commands_from_vps: true
`

func writeFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAddDeviceAppendsAndValidates(t *testing.T) {
	path := writeFixture(t, baseYAML)

	if err := AddDevice(path, "esp32c3-led", []string{"state", "command"}); err != nil {
		t.Fatalf("AddDevice() error = %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() after AddDevice error = %v", err)
	}
	if len(cfg.Devices) != 2 {
		t.Fatalf("len(devices) = %d, want 2", len(cfg.Devices))
	}
	added := cfg.Devices[1]
	if added.ID != "esp32c3-led" {
		t.Errorf("added.ID = %q", added.ID)
	}
	if added.Topics.State != "devices/esp32c3-led/state" || added.Topics.Command != "devices/esp32c3-led/command" {
		t.Errorf("added.Topics = %#v", added.Topics)
	}
	if added.Topics.Telemetry != "" || added.Topics.Event != "" || added.Topics.CommandResult != "" {
		t.Errorf("unselected topics should stay empty: %#v", added.Topics)
	}
	if added.Enabled == nil || !*added.Enabled {
		t.Errorf("added.Enabled = %v, want true", added.Enabled)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# comment that must survive a rewrite") {
		t.Errorf("rewrite dropped the leading comment:\n%s", raw)
	}
	if !strings.Contains(string(raw), "esp32-sala") {
		t.Errorf("rewrite dropped the original device:\n%s", raw)
	}

	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if string(backup) != baseYAML {
		t.Errorf("backup content changed, want exact original")
	}
}

func TestAddDeviceRejectsDuplicateID(t *testing.T) {
	path := writeFixture(t, baseYAML)
	err := AddDevice(path, "esp32-sala", []string{"state"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("AddDevice() error = %v, want already-exists", err)
	}
}

func TestAddDeviceRejectsUnknownTopic(t *testing.T) {
	path := writeFixture(t, baseYAML)
	err := AddDevice(path, "esp32c3-led", []string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown topic") {
		t.Fatalf("AddDevice() error = %v, want unknown-topic", err)
	}
}

func TestAddDeviceRejectsNoTopics(t *testing.T) {
	path := writeFixture(t, baseYAML)
	err := AddDevice(path, "esp32c3-led", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one topic") {
		t.Fatalf("AddDevice() error = %v, want at-least-one-topic", err)
	}
}

func TestAddDeviceRejectsInvalidID(t *testing.T) {
	path := writeFixture(t, baseYAML)
	err := AddDevice(path, "Not Valid", []string{"state"})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("AddDevice() error = %v, want invalid configuration", err)
	}
	// The invalid write must never land on disk.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "Not Valid") {
		t.Errorf("invalid device leaked into the file:\n%s", raw)
	}
}

func TestRemoveDeviceDeletesOnlyTheMatch(t *testing.T) {
	path := writeFixture(t, baseYAML)
	if err := AddDevice(path, "esp32c3-led", []string{"state", "command"}); err != nil {
		t.Fatal(err)
	}

	if err := RemoveDevice(path, "esp32-sala"); err != nil {
		t.Fatalf("RemoveDevice() error = %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() after RemoveDevice error = %v", err)
	}
	if len(cfg.Devices) != 1 || cfg.Devices[0].ID != "esp32c3-led" {
		t.Fatalf("devices after remove = %#v", cfg.Devices)
	}
}

func TestRemoveDeviceRejectsUnknownID(t *testing.T) {
	path := writeFixture(t, baseYAML)
	err := RemoveDevice(path, "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("RemoveDevice() error = %v, want not-found", err)
	}
}

func TestListDevices(t *testing.T) {
	path := writeFixture(t, baseYAML)
	devices, err := ListDevices(path)
	if err != nil {
		t.Fatalf("ListDevices() error = %v", err)
	}
	if len(devices) != 1 || devices[0].ID != "esp32-sala" {
		t.Fatalf("ListDevices() = %#v", devices)
	}
}
