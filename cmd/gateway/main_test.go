package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	gatewaymqtt "github.com/ricardossiqueira/iot-gateway/internal/mqtt"
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
	code := run([]string{"unknown"}, &stdout, &stderr)
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

func TestRunPublishTestCommand(t *testing.T) {
	t.Setenv("MQTT_USER", "gateway")
	t.Setenv("MQTT_PASSWORD", "secret")
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(commandConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &commandTestClient{}
	var stderr bytes.Buffer
	code := runPublishTestCommand([]string{"--config", path, "--device", "esp32-sala"}, &stderr, func(config.MQTT, gatewaymqtt.Credentials) (gatewaymqtt.Client, error) {
		return client, nil
	})
	if code != 0 {
		t.Fatalf("runPublishTestCommand() = %d, stderr = %s", code, stderr.String())
	}
	if !client.connected || !client.closed {
		t.Errorf("client lifecycle connected=%t closed=%t", client.connected, client.closed)
	}
	if !client.hasDeadline {
		t.Error("test command client did not receive a bounded context")
	}
	if client.topic != "devices/esp32-sala/command" || client.qos != 1 || client.retain {
		t.Errorf("publication topic=%q qos=%d retain=%t", client.topic, client.qos, client.retain)
	}
	var command struct {
		CommandID  string         `json:"command_id"`
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(client.payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != "gateway_test" || command.CommandID == "" || command.Parameters == nil {
		t.Errorf("command = %#v", command)
	}
}

type commandTestClient struct {
	connected   bool
	closed      bool
	topic       string
	payload     []byte
	qos         byte
	retain      bool
	hasDeadline bool
}

func (c *commandTestClient) Connect(ctx context.Context) error {
	c.connected = true
	deadline, ok := ctx.Deadline()
	c.hasDeadline = ok && time.Until(deadline) <= testCommandTimeout && time.Until(deadline) > 0
	return nil
}
func (c *commandTestClient) Subscribe(context.Context, string, gatewaymqtt.MessageHandler) error {
	return nil
}
func (c *commandTestClient) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	c.topic, c.payload, c.qos, c.retain = topic, payload, qos, retain
	return nil
}
func (c *commandTestClient) Close() { c.closed = true }

const validConfig = `gateway:
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

const commandConfig = `gateway:
  id: orangepi-lab-01
mqtt:
  url: mqtt://127.0.0.1:1883
  client_id: iot-gateway-orangepi-lab-01
  username_env: MQTT_USER
  password_env: MQTT_PASSWORD
storage:
  sqlite_path: /tmp/gateway.db
  max_outbox_messages: 100
  max_outbox_age: 24h
devices:
  - id: esp32-sala
    type: esp32
    enabled: true
    topics:
      telemetry: devices/esp32-sala/telemetry
      command: devices/esp32-sala/command
    forwarding:
      telemetry_to_vps: true
`
