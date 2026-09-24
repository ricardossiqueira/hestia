// Package config loads and validates the gateway's declarative configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Config is the complete gateway configuration file.
type Config struct {
	Gateway     Gateway     `yaml:"gateway"`
	MQTT        MQTT        `yaml:"mqtt"`
	Storage     Storage     `yaml:"storage"`
	Diagnostics Diagnostics `yaml:"diagnostics"`
	API         *API        `yaml:"api"`
	Devices     []Device    `yaml:"devices"`
	Routes      []Route     `yaml:"routes"`
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
	MaxOutboxBytes    int64    `yaml:"max_outbox_bytes"`
	MaxOutboxAge      Duration `yaml:"max_outbox_age"`
}

// Diagnostics configures the local, read-only operational HTTP endpoint.
// Empty values use the safe defaults applied by Parse.
type Diagnostics struct {
	Address        string   `yaml:"address"`
	RequestTimeout Duration `yaml:"request_timeout"`
	addressSet     bool
	timeoutSet     bool
}

const (
	defaultDiagnosticsAddress = "127.0.0.1:8080"
	defaultDiagnosticsTimeout = 2 * time.Second
	maxDiagnosticsTimeout     = 10 * time.Second
)

// API configures the optional Connect-RPC API composed on the LAN-reachable
// Address below. Unlike Diagnostics, it is opt-in: a nil *API (the "api:"
// key absent from the YAML) means neither process below starts anything
// for it.
//
// Two different processes read this same section (docs/decisions.md
// ADR-013): `iot-gateway admin` (root) binds Address and is the only
// public listener - it authenticates, applies CORS, and either answers a
// request itself (DeviceAdminService, a later phase) or reverse-proxies it
// (DeviceService/GatewayService) to `iot-gateway run` (sandboxed), which
// binds InternalAddress and does neither auth nor CORS: InternalAddress
// being loopback-only (validateAPI enforces this, unlike Address) IS its
// trust boundary. Never merge the two - see ADR-008 for why the sandboxed
// process must never itself be the public listener for anything that can
// mutate system state, and ADR-013 for why it does not even need its own
// credential once it is loopback-only and the only caller (the admin
// process) already authenticated the request at the public edge.
type API struct {
	// Address is the public, LAN-reachable address the admin process
	// binds. Deliberately no loopback restriction (see validateAPI).
	Address string `yaml:"address"`
	// InternalAddress is where `iot-gateway run` binds internal/api's
	// Connect-RPC server, reachable only by the admin process's reverse
	// proxy on the same host. Defaults to defaultAPIInternalAddress when
	// API is configured but this is omitted (applyDefaults). Must be a
	// loopback IP literal (validateAPI) and different from Address.
	InternalAddress string `yaml:"internal_address,omitempty"`
	// AllowedOrigins is an exact allowlist of browser origins (scheme +
	// host + optional port, e.g. "http://localhost:5173") permitted to
	// call Address cross-origin with credentials. Empty (the default)
	// means CORS is off: no Access-Control-* headers are ever added, and
	// a browser cannot call this API cross-origin at all - only
	// same-origin/non-browser clients (curl, a server) can. Never "*": a
	// credentialed CORS response requires echoing a specific, allowlisted
	// origin (see validateAPI and internal/apigateway's cors middleware).
	AllowedOrigins []string `yaml:"cors_allowed_origins,omitempty"`
}

const defaultAPIInternalAddress = "127.0.0.1:8083"

// Duration represents a Go duration written as a YAML string, such as "168h".
type Duration time.Duration

// UnmarshalYAML tracks whether optional diagnostics fields were specified, so
// a safe default never masks an explicitly invalid zero value.
func (d *Diagnostics) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return errors.New("diagnostics must be a mapping")
	}
	for index := 0; index < len(value.Content); index += 2 {
		switch value.Content[index].Value {
		case "address", "request_timeout":
		default:
			return fmt.Errorf("field %s not found in type config.Diagnostics", value.Content[index].Value)
		}
	}
	var raw struct {
		Address        *string   `yaml:"address"`
		RequestTimeout *Duration `yaml:"request_timeout"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	if raw.Address != nil {
		d.Address = *raw.Address
		d.addressSet = true
	}
	if raw.RequestTimeout != nil {
		d.RequestTimeout = *raw.RequestTimeout
		d.timeoutSet = true
	}
	return nil
}

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

// Topics uses omitempty on encode only (Parse never encodes) so that
// internal/admin can marshal a device with only the topics it actually
// uses, matching docs/device-onboarding.md's own examples instead of
// emitting all five keys with empty strings.
type Topics struct {
	Telemetry     string `yaml:"telemetry,omitempty"`
	State         string `yaml:"state,omitempty"`
	Event         string `yaml:"event,omitempty"`
	Command       string `yaml:"command,omitempty"`
	CommandResult string `yaml:"command_result,omitempty"`
}

type Forwarding struct {
	TelemetryToVPS  bool `yaml:"telemetry_to_vps"`
	StateToVPS      bool `yaml:"state_to_vps"`
	EventsToVPS     bool `yaml:"events_to_vps"`
	CommandsFromVPS bool `yaml:"commands_from_vps"`
}

// Route declares a device-agnostic local MQTT route.
type Route struct {
	ID               string         `yaml:"id"`
	SourceTopic      string         `yaml:"source_topic"`
	DestinationTopic string         `yaml:"destination_topic"`
	Transform        RouteTransform `yaml:"transform"`
	QoS              byte           `yaml:"qos"`
	Retain           bool           `yaml:"retain"`
}

// RouteTransform describes a generic payload conversion.
type RouteTransform struct {
	Type        string `yaml:"type"`
	CommandType string `yaml:"command_type"`
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
	applyDefaults(&c)

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func applyDefaults(c *Config) {
	if !c.Diagnostics.addressSet {
		c.Diagnostics.Address = defaultDiagnosticsAddress
	}
	if !c.Diagnostics.timeoutSet {
		c.Diagnostics.RequestTimeout = Duration(defaultDiagnosticsTimeout)
	}
	if c.API != nil && c.API.InternalAddress == "" {
		c.API.InternalAddress = defaultAPIInternalAddress
	}
}

// Validate enforces the routing contract documented in docs/configuration.md.
func (c Config) Validate() error {
	if err := ValidateDeviceID("gateway.id", c.Gateway.ID); err != nil {
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
	if c.Storage.MaxOutboxBytes <= 0 {
		return errors.New("storage.max_outbox_bytes must be greater than zero")
	}
	if c.Storage.MaxOutboxAge.TimeDuration() <= 0 {
		return errors.New("storage.max_outbox_age must be greater than zero")
	}
	if err := validateDiagnostics(c.Diagnostics); err != nil {
		return err
	}
	if c.API != nil {
		if err := validateAPI(*c.API); err != nil {
			return err
		}
	}
	deviceIDs := make(map[string]struct{}, len(c.Devices))
	topicOwners := make(map[string]string)
	inboundTopics := make(map[string]bool)
	commandTopics := make(map[string]bool)
	for index, device := range c.Devices {
		prefix := fmt.Sprintf("devices[%d]", index)
		if err := ValidateDeviceID(prefix+".id", device.ID); err != nil {
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
		if *device.Enabled {
			for _, topic := range []string{device.Topics.Telemetry, device.Topics.State, device.Topics.Event, device.Topics.CommandResult} {
				if topic != "" {
					inboundTopics[topic] = true
				}
			}
			if device.Topics.Command != "" {
				commandTopics[device.Topics.Command] = true
			}
		}
	}
	if err := validateRoutes(c.Routes, inboundTopics, commandTopics); err != nil {
		return err
	}
	return nil
}

func validateDiagnostics(diagnostics Diagnostics) error {
	host, portText, err := net.SplitHostPort(diagnostics.Address)
	if err != nil {
		return errors.New("diagnostics.address must be an IP literal and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("diagnostics.address must use a loopback IP literal")
	}
	port, err := strconv.Atoi(portText)
	if strings.Trim(portText, "0123456789") != "" || err != nil || port < 1 || port > 65535 {
		return errors.New("diagnostics.address must include a port between 1 and 65535")
	}
	timeout := diagnostics.RequestTimeout.TimeDuration()
	if timeout <= 0 || timeout > maxDiagnosticsTimeout {
		return errors.New("diagnostics.request_timeout must be greater than zero and at most 10s")
	}
	return nil
}

// validateAPI only runs when api is configured at all (see Config.API's
// doc comment). Address (the public, admin-bound listener) deliberately has
// no loopback restriction, unlike validateDiagnostics: it exists
// specifically to be reachable from elsewhere on the LAN. InternalAddress
// (the sandboxed, admin-only-reachable listener) is the opposite: it MUST
// be loopback, using the same check as validateDiagnostics, because that
// restriction is the only thing standing between the sandboxed process and
// being directly, unauthenticated-ly reachable from the LAN (ADR-013).
func validateAPI(api API) error {
	// host may legitimately be empty (e.g. ":8081", meaning all
	// interfaces) - unlike validateDiagnostics, there is no loopback (or
	// any other) restriction on it here.
	_, portText, err := net.SplitHostPort(api.Address)
	if err != nil {
		return errors.New("api.address must be a host (optional) and port")
	}
	port, err := strconv.Atoi(portText)
	if strings.Trim(portText, "0123456789") != "" || err != nil || port < 1 || port > 65535 {
		return errors.New("api.address must include a port between 1 and 65535")
	}

	internalHost, internalPortText, err := net.SplitHostPort(api.InternalAddress)
	if err != nil {
		return errors.New("api.internal_address must be an IP literal and port")
	}
	internalIP := net.ParseIP(internalHost)
	if internalIP == nil || !internalIP.IsLoopback() {
		return errors.New("api.internal_address must use a loopback IP literal")
	}
	internalPort, err := strconv.Atoi(internalPortText)
	if strings.Trim(internalPortText, "0123456789") != "" || err != nil || internalPort < 1 || internalPort > 65535 {
		return errors.New("api.internal_address must include a port between 1 and 65535")
	}
	if api.Address == api.InternalAddress {
		return errors.New("api.address and api.internal_address must be different")
	}

	for _, origin := range api.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return fmt.Errorf("api.cors_allowed_origins: %w", err)
		}
	}
	return nil
}

// validateOrigin rejects anything that is not exactly a browser Origin
// header value: scheme http/https, a host, and nothing else - no path,
// query, fragment, userinfo, or the literal "*". A credentialed CORS
// response (Access-Control-Allow-Credentials: true) must echo one specific
// allowlisted origin; the browser rejects "*" for credentialed requests
// anyway, but rejecting it here fails fast at `iot-gateway validate`
// instead of silently never working from a browser.
func validateOrigin(origin string) error {
	if origin == "*" {
		return errors.New(`"*" is not allowed - list exact origins`)
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("%q is not a valid origin: %w", origin, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%q must use http or https", origin)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%q must include a host", origin)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("%q must be exactly scheme://host[:port], no path/query/fragment/credentials", origin)
	}
	return nil
}

func validateRoutes(routes []Route, inboundTopics, commandTopics map[string]bool) error {
	ids := make(map[string]struct{}, len(routes))
	for index, route := range routes {
		prefix := fmt.Sprintf("routes[%d]", index)
		if err := ValidateDeviceID(prefix+".id", route.ID); err != nil {
			return err
		}
		if _, exists := ids[route.ID]; exists {
			return fmt.Errorf("duplicate route ID %q", route.ID)
		}
		ids[route.ID] = struct{}{}
		if !inboundTopics[route.SourceTopic] {
			return fmt.Errorf("%s.source_topic must reference an enabled inbound device topic", prefix)
		}
		if !commandTopics[route.DestinationTopic] {
			return fmt.Errorf("%s.destination_topic must reference an enabled command topic", prefix)
		}
		if route.QoS > 2 {
			return fmt.Errorf("%s.qos must be between 0 and 2", prefix)
		}
		if route.Transform.Type != "json_command" {
			return fmt.Errorf("%s.transform.type must be json_command", prefix)
		}
		if strings.TrimSpace(route.Transform.CommandType) == "" {
			return fmt.Errorf("%s.transform.command_type is required", prefix)
		}
	}
	return nil
}

// ValidateDeviceID enforces the same ID rule this package uses for every
// gateway/device/route identifier - exported so internal/admin can apply
// the identical rule before provisioning a device through the API,
// instead of duplicating the regex and risking it drifting from what
// `iot-gateway validate` actually accepts.
func ValidateDeviceID(field, id string) error {
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
	usernameEnv := strings.TrimSpace(mqtt.UsernameEnv)
	passwordEnv := strings.TrimSpace(mqtt.PasswordEnv)
	if usernameEnv == "" || passwordEnv == "" {
		return errors.New("mqtt.username_env and mqtt.password_env are required and must be configured together")
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
