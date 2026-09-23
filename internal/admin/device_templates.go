package admin

// DeviceTemplate is one compiled provisioning template: the fixed shape of
// a device that ProvisionDevice (registration.go) creates from just an ID.
// Same spirit as internal/deviceprofile's registry (a plain Go map, not a
// file read or a YAML-configurable registry) - "esp32_led.v1" is the only
// entry for now, per gateway-web/docs/spec.md section 8.1 ("inicialmente
// somente esp32_led.v1").
type DeviceTemplate struct {
	// Type describes hardware, same meaning as config.Device.Type.
	Type string
	// Profile names an internal/deviceprofile registry entry (empty means
	// no schema validation for this device's commands - see
	// config.Device.Profile's doc comment). esp32_led.v1 sets "led.v1" so
	// a provisioned LED gets schema-validated set_led commands for free.
	Profile string
	// Topics are the topic suffixes to create, same vocabulary as
	// AddDevice's topicSuffixes (validTopicSuffixes in devices.go).
	Topics []string
	// AdoptExisting means this template belongs to a local service whose
	// DynSec identity is provisioned independently. It must use
	// RegisterExistingDevice, never ProvisionDevice (which rotates a secret).
	AdoptExisting bool
}

// deviceTemplates is the compiled registry. Adding a template is a
// one-line, greppable change here, not new infrastructure.
var deviceTemplates = map[string]DeviceTemplate{
	"esp32_led.v1": {
		Type:    "esp32",
		Profile: "led.v1",
		Topics:  []string{"state", "command"},
	},
	// CYD renders the generic render_system_status contract in its firmware.
	// It has no Protobuf command profile yet, so the gateway accepts its
	// command payload as the existing opaque-device contract.
	"cyd_monitor.v1": {
		Type:   "esp32-cyd",
		Topics: []string{"command"},
	},
	"orangepi_monitor.v1": {
		Type:          "linux-system-monitor",
		Topics:        []string{"telemetry"},
		AdoptExisting: true,
	},
}

// TemplateExists reports whether name names a registry entry.
// ProvisionDevice (registration.go) calls this before touching Mosquitto
// or gateway.yaml, so an unknown template name fails immediately instead
// of after a credential was already created.
func TemplateExists(name string) bool {
	_, ok := deviceTemplates[name]
	return ok
}
