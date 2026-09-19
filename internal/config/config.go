// Package config loads and validates the gateway's declarative configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Config is the complete gateway configuration file.
type Config struct {
	Gateway Gateway  `yaml:"gateway"`
	MQTT    MQTT     `yaml:"mqtt"`
	Storage Storage  `yaml:"storage"`
	Devices []Device `yaml:"devices"`
}

type Gateway struct {
	ID       string `yaml:"id"`
	Timezone string `yaml:"timezone"`
}

type MQTT struct {
	URL         string `yaml:"url"`
	ClientID    string `yaml:"client_id"`
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`
}

type Storage struct {
	SQLitePath        string   `yaml:"sqlite_path"`
	MaxOutboxMessages int      `yaml:"max_outbox_messages"`
	MaxOutboxAge      Duration `yaml:"max_outbox_age"`
}

// Duration represents a Go duration written as a YAML string, such as "168h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return errors.New("duration must be a string")
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) TimeDuration() time.Duration { return time.Duration(d) }

type Device struct {
	ID         string     `yaml:"id"`
	Type       string     `yaml:"type"`
	Enabled    *bool      `yaml:"enabled"`
	Topics     Topics     `yaml:"topics"`
	Forwarding Forwarding `yaml:"forwarding"`
}

type Topics struct {
	Telemetry     string `yaml:"telemetry"`
	State         string `yaml:"state"`
	Event         string `yaml:"event"`
	Command       string `yaml:"command"`
	CommandResult string `yaml:"command_result"`
}

type Forwarding struct {
	TelemetryToVPS  bool `yaml:"telemetry_to_vps"`
	StateToVPS      bool `yaml:"state_to_vps"`
	EventsToVPS     bool `yaml:"events_to_vps"`
	CommandsFromVPS bool `yaml:"commands_from_vps"`
}

// Load reads, parses and validates a YAML configuration file.
func Load(path string) (Config, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration %q: %w", path, err)
	}
	return Parse(contents)
}

// Parse decodes and validates YAML configuration. Unknown fields are rejected so
// configuration mistakes never silently change the gateway's behavior.
func Parse(contents []byte) (Config, error) {
	if len(bytes.TrimSpace(contents)) == 0 {
		return Config{}, errors.New("configuration is empty")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	var c Config
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode YAML configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("configuration must contain one YAML document")
		}
		return Config{}, fmt.Errorf("decode YAML configuration: %w", err)
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate enforces the routing contract documented in docs/configuration.md.
func (c Config) Validate() error {
	if err := validateID("gateway.id", c.Gateway.ID); err != nil {
		return err
	}
	if c.Gateway.Timezone != "" {
		if _, err := time.LoadLocation(c.Gateway.Timezone); err != nil {
			return fmt.Errorf("gateway.timezone is invalid: %w", err)
		}
	}
	if err := validateMQTT(c.MQTT); err != nil {
		return err
	}
	if strings.TrimSpace(c.Storage.SQLitePath) == "" {
		return errors.New("storage.sqlite_path is required")
	}
	if c.Storage.MaxOutboxMessages <= 0 {
		return errors.New("storage.max_outbox_messages must be greater than zero")
	}
	if c.Storage.MaxOutboxAge.TimeDuration() <= 0 {
		return errors.New("storage.max_outbox_age must be greater than zero")
	}
	if len(c.Devices) == 0 {
		return errors.New("configuration must define at least one device")
	}

	deviceIDs := make(map[string]struct{}, len(c.Devices))
	topicOwners := make(map[string]string)
	for index, device := range c.Devices {
		prefix := fmt.Sprintf("devices[%d]", index)
		if err := validateID(prefix+".id", device.ID); err != nil {
			return err
		}
		if _, exists := deviceIDs[device.ID]; exists {
			return fmt.Errorf("duplicate device ID %q", device.ID)
		}
		deviceIDs[device.ID] = struct{}{}
		if strings.TrimSpace(device.Type) == "" {
			return fmt.Errorf("%s.type is required", prefix)
		}
		if device.Enabled == nil {
			return fmt.Errorf("%s.enabled is required", prefix)
		}

		if err := validateTopics(prefix, device, topicOwners); err != nil {
			return err
		}
		if err := validateForwarding(prefix, device); err != nil {
			return err
		}
	}
	return nil
}

func validateID(field, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%s must contain only lowercase letters, digits, hyphens or underscores", field)
	}
	return nil
}

func validateMQTT(mqtt MQTT) error {
	if strings.TrimSpace(mqtt.URL) == "" {
		return errors.New("mqtt.url is required")
	}
	parsed, err := url.Parse(mqtt.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("mqtt.url is invalid")
	}
	if parsed.Scheme != "mqtt" && parsed.Scheme != "mqtts" {
		return errors.New("mqtt.url must use mqtt or mqtts")
	}
	if parsed.Port() == "" {
		return errors.New("mqtt.url must include a port")
	}
	if strings.TrimSpace(mqtt.ClientID) == "" {
		return errors.New("mqtt.client_id is required")
	}
	return nil
}

func validateTopics(prefix string, device Device, owners map[string]string) error {
	topics := []struct {
		name   string
		value  string
		suffix string
	}{
		{"telemetry", device.Topics.Telemetry, "telemetry"},
		{"state", device.Topics.State, "state"},
		{"event", device.Topics.Event, "event"},
		{"command", device.Topics.Command, "command"},
		{"command_result", device.Topics.CommandResult, "command-result"},
	}

	count := 0
	for _, topic := range topics {
		if topic.value == "" {
			continue
		}
		count++
		field := prefix + ".topics." + topic.name
		if strings.ContainsAny(topic.value, "+#") {
			return fmt.Errorf("%s must not contain MQTT wildcards", field)
		}
		if owner, exists := owners[topic.value]; exists {
			return fmt.Errorf("%s topic %q is also assigned to %s", field, topic.value, owner)
		}
		devicePrefix := "devices/" + device.ID + "/"
		if !strings.HasPrefix(topic.value, devicePrefix) {
			return fmt.Errorf("%s must start with %q", field, devicePrefix)
		}
		expected := devicePrefix + topic.suffix
		if topic.value != expected {
			return fmt.Errorf("%s must be %q", field, expected)
		}
		owners[topic.value] = field
	}
	if count == 0 {
		return fmt.Errorf("%s.topics must define at least one topic", prefix)
	}
	return nil
}

func validateForwarding(prefix string, device Device) error {
	checks := []struct {
		enabled bool
		topic   string
		field   string
		kind    string
	}{
		{device.Forwarding.TelemetryToVPS, device.Topics.Telemetry, "telemetry_to_vps", "telemetry"},
		{device.Forwarding.StateToVPS, device.Topics.State, "state_to_vps", "state"},
		{device.Forwarding.EventsToVPS, device.Topics.Event, "events_to_vps", "event"},
		{device.Forwarding.CommandsFromVPS, device.Topics.Command, "commands_from_vps", "command"},
	}
	for _, check := range checks {
		if check.enabled && check.topic == "" {
			return fmt.Errorf("%s.forwarding.%s requires a %s topic", prefix, check.field, check.kind)
		}
	}
	return nil
}
