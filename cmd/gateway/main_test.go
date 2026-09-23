package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestDynsecCredentialsFromEnvironment(t *testing.T) {
	passwordPath := filepath.Join(t.TempDir(), "dynsec-password")
	if err := os.WriteFile(passwordPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IOT_GATEWAY_DYNSEC_URL", "tcp://127.0.0.1:1884")
	t.Setenv("IOT_GATEWAY_DYNSEC_ADMIN_USERNAME", "gateway-control")
	t.Setenv("IOT_GATEWAY_DYNSEC_ADMIN_PASSWORD_FILE", passwordPath)
	store, endpoint, err := dynsecCredentialsFromEnvironment()
	if err != nil || store == nil || endpoint != "tcp://127.0.0.1:1884" {
		t.Fatalf("dynsecCredentialsFromEnvironment() = %T, %q, %v", store, endpoint, err)
	}
}

func TestSameMQTTEndpoint(t *testing.T) {
	if !sameMQTTEndpoint("mqtt://127.0.0.1:1884", "tcp://127.0.0.1:1884") {
		t.Fatal("equivalent endpoints did not match")
	}
	if sameMQTTEndpoint("mqtt://127.0.0.1:1883", "tcp://127.0.0.1:1884") {
		t.Fatal("different ports matched")
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

func TestRunHealthcheckUsesOnlyLocalDiagnostics(t *testing.T) {
	var receivedPath, receivedQuery, receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedPath = request.URL.Path
		receivedQuery = request.URL.RawQuery
		receivedMethod = request.Method
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	t.Setenv("MQTT_USER", "")
	t.Setenv("MQTT_PASSWORD", "")
	path := writeHealthcheckConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"healthcheck", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "healthcheck is ok\n" {
		t.Errorf("stdout = %q", got)
	}
	if receivedMethod != http.MethodGet || receivedPath != "/healthz" || receivedQuery != "" {
		t.Errorf("request = %s %s?%s", receivedMethod, receivedPath, receivedQuery)
	}
}

func TestRunHealthcheckRejectsUnavailableResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"status":"unavailable"}`))
	}))
	defer server.Close()

	path := writeHealthcheckConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "healthcheck failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunHealthcheckRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`not-json`))
	}))
	defer server.Close()

	path := writeHealthcheckConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "healthcheck failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunHealthcheckRequiresOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"unavailable"}`))
	}))
	defer server.Close()

	path := writeHealthcheckConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "healthcheck failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunHealthcheckRejectsRedirect(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect-target" {
			redirected = true
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Redirect(writer, request, "/redirect-target", http.StatusFound)
	}))
	defer server.Close()

	path := writeHealthcheckConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "--config", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if redirected {
		t.Error("healthcheck followed a redirect")
	}
}

func writeHealthcheckConfig(t *testing.T, address string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	contents := strings.Replace(validConfig, "/var/lib/iot-gateway/gateway.db", filepath.Join(t.TempDir(), "missing", "gateway.db"), 1)
	contents += fmt.Sprintf("diagnostics:\n  address: %s\n  request_timeout: 1s\n", address)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPublishTestCommand(t *testing.T) {
	t.Setenv("MQTT_USER", "gateway")
	t.Setenv("MQTT_PASSWORD", "secret")
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "gateway.yaml")
	contents := strings.Replace(commandConfig, "/tmp/gateway.db", filepath.Join(tempDir, "gateway.db"), 1)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
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
func (c *commandTestClient) Connected() bool { return c.connected && !c.closed }
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
  max_outbox_bytes: 1048576
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
  max_outbox_bytes: 1048576
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
