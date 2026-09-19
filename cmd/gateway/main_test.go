package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "configuration is valid\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"start"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunValidateUsesDefaultConfigPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "configuration is invalid") {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunValidateRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(path, []byte("gateway: ["), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", path}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "configuration is invalid") {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
}

const validConfig = `gateway:
  id: orangepi-lab-01
  timezone: America/Sao_Paulo
mqtt:
  url: mqtt://127.0.0.1:1883
  client_id: iot-gateway-orangepi-lab-01
storage:
  sqlite_path: /var/lib/iot-gateway/gateway.db
  max_outbox_messages: 100
  max_outbox_age: 24h
devices:
  - id: esp32-sala
    type: esp32
    enabled: true
    topics:
      telemetry: devices/esp32-sala/telemetry
    forwarding:
      telemetry_to_vps: true
`
