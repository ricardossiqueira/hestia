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
	if config.Diagnostics.Address != "127.0.0.1:8080" || config.Diagnostics.RequestTimeout.TimeDuration().String() != "2s" {
		t.Errorf("diagnostics defaults = %#v", config.Diagnostics)
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
			name: "missing outbox byte limit",
			yaml: strings.Replace(validYAML, "  max_outbox_bytes: 1048576\n", "", 1),
			want: "storage.max_outbox_bytes must be greater than zero",
		},
		{
			name: "zero outbox byte limit",
			yaml: strings.Replace(validYAML, "max_outbox_bytes: 1048576", "max_outbox_bytes: 0", 1),
			want: "storage.max_outbox_bytes must be greater than zero",
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
		{
			name: "diagnostics hostname is forbidden",
			yaml: validYAML + "\ndiagnostics:\n  address: localhost:8080\n",
			want: "loopback IP literal",
		},
		{
			name: "diagnostics non loopback is forbidden",
			yaml: validYAML + "\ndiagnostics:\n  address: 0.0.0.0:8080\n",
			want: "loopback IP literal",
		},
		{
			name: "diagnostics port is required",
			yaml: validYAML + "\ndiagnostics:\n  address: 127.0.0.1\n",
			want: "IP literal and port",
		},
		{
			name: "diagnostics port must be decimal digits",
			yaml: validYAML + "\ndiagnostics:\n  address: 127.0.0.1:+8080\n",
			want: "between 1 and 65535",
		},
		{
			name: "diagnostics timeout is bounded",
			yaml: validYAML + "\ndiagnostics:\n  request_timeout: 11s\n",
			want: "at most 10s",
		},
		{
			name: "explicit zero diagnostics timeout is rejected",
			yaml: validYAML + "\ndiagnostics:\n  request_timeout: 0s\n",
			want: "must be greater than zero",
		},
		{
			name: "unknown diagnostics field",
			yaml: validYAML + "\ndiagnostics:\n  unsafe: true\n",
			want: "field unsafe not found",
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

func TestParseAcceptsLoopbackDiagnosticsConfiguration(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + `
diagnostics:
  address: "[::1]:9090"
  request_timeout: 5s
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Diagnostics.Address != "[::1]:9090" || cfg.Diagnostics.RequestTimeout.TimeDuration().String() != "5s" {
		t.Errorf("diagnostics = %#v", cfg.Diagnostics)
	}
}

func TestParseAPIIsOptional(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API != nil {
		t.Errorf("API = %#v, want nil when absent from YAML", cfg.API)
	}
}

func TestParseAcceptsAPIConfiguration(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + `
api:
  address: "0.0.0.0:8081"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API == nil || cfg.API.Address != "0.0.0.0:8081" {
		t.Errorf("API = %#v", cfg.API)
	}
}

func TestParseAcceptsAPIWithoutHost(t *testing.T) {
	// ":8081" (no host) means "all interfaces" - unlike diagnostics.address,
	// there is no loopback restriction to enforce here.
	if _, err := Parse([]byte(validYAML + "\napi:\n  address: \":8081\"\n")); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseRejectsInvalidAPIAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
	}{
		{"missing port", "0.0.0.0"},
		{"non-numeric port", "0.0.0.0:abc"},
		{"port out of range", "0.0.0.0:70000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(validYAML + "\napi:\n  address: \"" + test.address + "\"\n"))
			if err == nil {
				t.Fatalf("Parse() with api.address = %q: want error", test.address)
			}
		})
	}
}

func TestParseAcceptsValidProfile(t *testing.T) {
	yaml := strings.Replace(validYAML, "type: esp32\n", "type: esp32\n    profile: led.v1\n", 1)
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Devices[0].Profile != "led.v1" {
		t.Errorf("Profile = %q, want led.v1", cfg.Devices[0].Profile)
	}
}

func TestParseRejectsUnknownProfile(t *testing.T) {
	yaml := strings.Replace(validYAML, "type: esp32\n", "type: esp32\n    profile: thermostat.v1\n", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "not a known device profile") {
		t.Fatalf("Parse() error = %v, want unknown profile error", err)
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

func TestParseAcceptsGenericRoute(t *testing.T) {
	yaml := validYAML + `
  - id: display
    type: display
    enabled: true
    topics:
      command: devices/display/command
routes:
  - id: telemetry-to-display
    source_topic: devices/esp32-sala/telemetry
    destination_topic: devices/display/command
    transform:
      type: json_command
      command_type: render_status
    qos: 1
    retain: false
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Transform.CommandType != "render_status" {
		t.Fatalf("routes = %#v", cfg.Routes)
	}
}

func TestParseRejectsRouteOutsideDeclaredEndpoints(t *testing.T) {
	yaml := validYAML + `
routes:
  - id: invalid-route
    source_topic: devices/unknown/telemetry
    destination_topic: devices/esp32-sala/command
    transform:
      type: json_command
      command_type: render_status
    qos: 1
    retain: false
`
	if _, err := Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), "enabled inbound device topic") {
		t.Fatalf("Parse() error = %v", err)
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
  max_outbox_bytes: 1048576
  max_outbox_age: 24h
` + devicesYAML
