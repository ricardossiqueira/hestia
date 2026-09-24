package admin

// DeviceTemplate is the fixed shape used only to adopt pre-existing local
// services. New network devices are described by published manifests.
type DeviceTemplate struct {
	// Type describes hardware, same meaning as config.Device.Type.
	Type string
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

// TemplateExists reports whether name names a legacy adoption entry.
func TemplateExists(name string) bool {
	_, ok := deviceTemplates[name]
	return ok
}
