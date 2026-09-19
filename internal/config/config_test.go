package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsValidConfiguration(t *testing.T) {
	config, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if config.Gateway.ID != "orangepi-lab-01" {
		t.Errorf("gateway ID = %q, want %q", config.Gateway.ID, "orangepi-lab-01")
	}
	if got := config.Devices[0].Topics.Command; got != "devices/esp32-sala/command" {
		t.Errorf("command topic = %q", got)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "read configuration") {
		t.Fatalf("Load() error = %v, want read error", err)
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing gateway ID",
			yaml: strings.Replace(validYAML, "id: orangepi-lab-01", "id: ", 1),
			want: "gateway.id is required",
		},
		{
			name: "invalid gateway ID",
			yaml: strings.Replace(validYAML, "id: orangepi-lab-01", "id: orange pi", 1),
			want: "gateway.id",
		},
		{
			name: "no devices",
			yaml: strings.Replace(validYAML, devicesYAML, "devices: []", 1),
			want: "at least one device",
		},
		{
			name: "missing device ID",
			yaml: strings.Replace(validYAML, "id: esp32-sala", "id: ", 1),
			want: "devices[0].id is required",
		},
		{
			name: "invalid device ID",
			yaml: strings.Replace(validYAML, "id: esp32-sala", "id: esp32/sala", 1),
			want: "devices[0].id",
		},
		{
			name: "missing device type",
			yaml: strings.Replace(validYAML, "type: esp32", "type: ", 1),
			want: "devices[0].type is required",
		},
		{
			name: "missing device enabled flag",
			yaml: strings.Replace(validYAML, "    enabled: true\n", "", 1),
			want: "devices[0].enabled is required",
		},
		{
			name: "no topics",
			yaml: strings.Replace(validYAML, "      telemetry: devices/esp32-sala/telemetry\n      state: devices/esp32-sala/state\n      event: devices/esp32-sala/event\n      command: devices/esp32-sala/command\n      command_result: devices/esp32-sala/command-result\n", "", 1),
			want: "topics must define at least one topic",
		},
		{
			name: "duplicate device ID",
			yaml: validYAML + `
  - id: esp32-sala
    type: esp32
    enabled: true
    topics:
      telemetry: devices/esp32-sala-2/telemetry
`,
			want: "duplicate device ID",
		},
		{
			name: "topic does not belong to device",
			yaml: strings.Replace(validYAML, "devices/esp32-sala/telemetry", "devices/esp32-outra/telemetry", 1),
			want: "must start with \"devices/esp32-sala/\"",
		},
		{
			name: "topic kind must match its field",
			yaml: strings.Replace(validYAML, "devices/esp32-sala/telemetry", "devices/esp32-sala/other", 1),
			want: "must be \"devices/esp32-sala/telemetry\"",
		},
		{
			name: "topic wildcard",
			yaml: strings.Replace(validYAML, "devices/esp32-sala/telemetry", "devices/esp32-sala/+", 1),
			want: "must not contain MQTT wildcards",
		},
		{
			name: "invalid MQTT URL",
			yaml: strings.Replace(validYAML, "mqtt://127.0.0.1:1883", "http://127.0.0.1:1883", 1),
			want: "mqtt.url must use mqtt or mqtts",
		},
		{
			name: "missing MQTT client ID",
			yaml: strings.Replace(validYAML, "client_id: iot-gateway-orangepi-lab-01", "client_id: ", 1),
			want: "mqtt.client_id is required",
		},
		{
			name: "unpaired MQTT credential environment variables",
			yaml: strings.Replace(validYAML, "  password_env: MQTT_PASSWORD\n", "", 1),
			want: "mqtt.username_env and mqtt.password_env are required",
		},
		{
			name: "missing SQLite path",
			yaml: strings.Replace(validYAML, "sqlite_path: /var/lib/iot-gateway/gateway.db", "sqlite_path: ", 1),
			want: "storage.sqlite_path is required",
		},
		{
			name: "zero outbox message limit",
			yaml: strings.Replace(validYAML, "max_outbox_messages: 100", "max_outbox_messages: 0", 1),
			want: "storage.max_outbox_messages must be greater than zero",
		},
		{
			name: "zero outbox age limit",
			yaml: strings.Replace(validYAML, "max_outbox_age: 24h", "max_outbox_age: 0s", 1),
			want: "storage.max_outbox_age must be greater than zero",
		},
		{
			name: "invalid outbox age",
			yaml: strings.Replace(validYAML, "max_outbox_age: 24h", "max_outbox_age: yesterday", 1),
			want: "invalid duration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if err == nil {
				t.Fatal("Parse() error = nil")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("Parse() error = %q, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestParseAcceptsEmptyTimezone(t *testing.T) {
	withoutTimezone := strings.Replace(validYAML, "  timezone: America/Sao_Paulo\n", "", 1)
	if _, err := Parse([]byte(withoutTimezone)); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseAcceptsExplicitlyDisabledDevice(t *testing.T) {
	disabled := strings.Replace(validYAML, "enabled: true", "enabled: false", 1)
	config, err := Parse([]byte(disabled))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if config.Devices[0].Enabled == nil || *config.Devices[0].Enabled {
		t.Fatalf("enabled = %v, want explicit false", config.Devices[0].Enabled)
	}
}

func TestParseRejectsForwardingWithoutItsTopic(t *testing.T) {
	tests := []struct {
		name      string
		topicLine string
		want      string
	}{
		{"telemetry", "      telemetry: devices/esp32-sala/telemetry\n", "telemetry_to_vps requires a telemetry topic"},
		{"state", "      state: devices/esp32-sala/state\n", "state_to_vps requires a state topic"},
		{"event", "      event: devices/esp32-sala/event\n", "events_to_vps requires a event topic"},
		{"command", "      command: devices/esp32-sala/command\n", "commands_from_vps requires a command topic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(validYAML, test.topicLine, "", 1)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsMalformedOrEmptyYAML(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty", yaml: " \n\t", want: "configuration is empty"},
		{name: "malformed", yaml: "gateway: [", want: "decode YAML"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(validYAML + "\nunknown: value\n"))
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("Parse() error = %v, want unknown-field error", err)
	}
}

func TestParseRejectsMultipleDocumentsAndNestedUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"multiple documents", validYAML + "\n---\ngateway:\n  id: another\n", "one YAML document"},
		{"nested unknown", strings.Replace(validYAML, "gateway:\n", "gateway:\n  extra: value\n", 1), "field extra not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

const devicesYAML = `devices:
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
      commands_from_vps: true`

const validYAML = `gateway:
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
` + devicesYAML
