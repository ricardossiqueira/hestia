package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestParseAPIInternalAddressDefaultsWhenOmitted(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + "\napi:\n  address: \"0.0.0.0:8082\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.InternalAddress != "127.0.0.1:8083" {
		t.Errorf("InternalAddress = %q, want the default", cfg.API.InternalAddress)
	}
}

func TestParseAcceptsExplicitInternalAddress(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + "\napi:\n  address: \"0.0.0.0:8082\"\n  internal_address: \"127.0.0.1:9999\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.InternalAddress != "127.0.0.1:9999" {
		t.Errorf("InternalAddress = %q, want the explicit value", cfg.API.InternalAddress)
	}
}

func TestParseRejectsInvalidInternalAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
	}{
		{"not loopback", "0.0.0.0:8083"},
		{"hostname, not an IP literal", "localhost:8083"},
		{"missing port", "127.0.0.1"},
		{"non-numeric port", "127.0.0.1:abc"},
		{"port out of range", "127.0.0.1:70000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := validYAML + "\napi:\n  address: \"0.0.0.0:8082\"\n  internal_address: \"" + test.address + "\"\n"
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatalf("Parse() with internal_address = %q: want error", test.address)
			}
		})
	}
}

func TestParseRejectsInternalAddressEqualToAddress(t *testing.T) {
	yaml := validYAML + "\napi:\n  address: \"127.0.0.1:8083\"\n  internal_address: \"127.0.0.1:8083\"\n"
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("Parse() with address == internal_address: want error")
	}
}

func TestParseAPIAllowedOriginsIsOptional(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + "\napi:\n  address: \"0.0.0.0:8081\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.API.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %#v, want empty when absent from YAML", cfg.API.AllowedOrigins)
	}
}

func TestParseAcceptsValidAllowedOrigins(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + `
api:
  address: "0.0.0.0:8081"
  cors_allowed_origins:
    - "http://localhost:5173"
    - "http://127.0.0.1:5173"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	if !reflect.DeepEqual(cfg.API.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins = %#v, want %#v", cfg.API.AllowedOrigins, want)
	}
}

func TestParseRejectsInvalidAllowedOrigins(t *testing.T) {
	tests := []struct {
		name   string
		origin string
	}{
		{"wildcard", "*"},
		{"missing scheme", "localhost:5173"},
		{"non-http scheme", "ftp://localhost:5173"},
		{"has a path", "http://localhost:5173/app"},
		{"has a query", "http://localhost:5173?x=1"},
		{"has credentials", "http://user:pass@localhost:5173"},
		{"not a URL at all", "not a url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := validYAML + "\napi:\n  address: \"0.0.0.0:8081\"\n  cors_allowed_origins:\n    - \"" + test.origin + "\"\n"
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatalf("Parse() with cors_allowed_origins = %q: want error", test.origin)
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
`
