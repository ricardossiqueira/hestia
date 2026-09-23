// Package admin implements device administration: provisioning a device
// from a template, enabling/disabling it, and removing it - each of these
// requires writing gateway.yaml and Mosquitto's credential/ACL files and
// calling systemctl (docs/decisions.md ADR-008), so this is the only part
// of this codebase that touches any of that. Exposed to the LAN by
// internal/apigateway's DeviceAdminService, which is the only caller
// (*Server structurally satisfies its DeviceAdmin interface). A JSON/HTML
// UI on port 8081 used to live here too; it was retired once gateway-web
// reached parity with it - see ADR-015.
package admin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"gopkg.in/yaml.v3"
)

// validTopicSuffixes mirrors config.Topics' YAML keys exactly - the only
// names a caller may request.
var validTopicSuffixes = map[string]struct{}{
	"telemetry":      {},
	"state":          {},
	"event":          {},
	"command":        {},
	"command_result": {},
}

// ListDevices returns the currently configured devices for display. It goes
// through the ordinary typed loader (which also validates), unlike
// AddDevice/RemoveDevice below - listing needs no special care to preserve
// file formatting, only writing does.
func ListDevices(path string) ([]config.Device, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return cfg.Devices, nil
}

// DeviceExists reports whether a device with the given id is already
// declared in the YAML file at path. Callers that are about to provision a
// new credential (an operation that ROTATES an existing device's password
// as a side effect - see Provision's doc comment) must check this first:
// calling Provision for an id that already exists and only discovering the
// YAML conflict afterwards silently invalidates a working device's
// password with no way to recover it from the response.
func DeviceExists(path, id string) (bool, error) {
	_, doc, err := readDocument(path)
	if err != nil {
		return false, err
	}
	devicesSeq, err := findDevicesSequence(doc)
	if err != nil {
		return false, err
	}
	for _, item := range devicesSeq.Content {
		if deviceNodeID(item) == id {
			return true, nil
		}
	}
	return false, nil
}

// AddDevice appends a new device entry to the YAML file at path, built from
// id and topicSuffixes (a non-empty subset of telemetry/state/event/
// command/command_result). It edits the file's yaml.Node tree rather than
// unmarshal-then-remarshal the typed Config, so comments and formatting
// elsewhere in a hand-maintained gateway.yaml survive untouched - only the
// devices sequence gains one element. The result is validated with
// config.Parse before anything is written; the previous content is kept at
// path+".bak".
func AddDevice(path, id string, topicSuffixes []string) error {
	if len(topicSuffixes) == 0 {
		return errors.New("at least one topic is required")
	}
	for _, suffix := range topicSuffixes {
		if _, ok := validTopicSuffixes[suffix]; !ok {
			return fmt.Errorf("unknown topic %q", suffix)
		}
	}

	// "esp32", "" reproduce exactly what buildDeviceNode hardcoded before
	// it grew deviceType/profile parameters for AddDeviceFromTemplate below
	// - this caller's behavior is unchanged.
	node, _, err := buildDeviceNode(id, "esp32", "", topicSuffixes)
	if err != nil {
		return err
	}
	return insertDeviceNode(path, id, node)
}

// AddDeviceFromTemplate is AddDevice's templated counterpart, used by
// RegisterDevice (registration.go): the type, profile and topics all come
// from a compiled DeviceTemplate (device_templates.go) instead of being
// chosen per call. Returns the device it just created (so a caller like
// RegisterDevice doesn't need a second config.Load to report it back) and
// an error for an unknown template name - callers should normally have
// already checked TemplateExists earlier in their flow, before touching
// Mosquitto, so this is a defensive fallback.
func AddDeviceFromTemplate(path, id, templateName string) (config.Device, error) {
	tmpl, ok := deviceTemplates[templateName]
	if !ok {
		return config.Device{}, fmt.Errorf("unknown template %q", templateName)
	}
	node, device, err := buildDeviceNode(id, tmpl.Type, tmpl.Profile, tmpl.Topics)
	if err != nil {
		return config.Device{}, err
	}
	if err := insertDeviceNode(path, id, node); err != nil {
		return config.Device{}, err
	}
	return device, nil
}

// insertDeviceNode is AddDevice and AddDeviceFromTemplate's shared tail:
// load the document, reject a duplicate id, append node, and write back
// validated. Node-building (buildDeviceNode) happens before this is called
// because each caller shapes the device differently; everything after
// that point is identical.
func insertDeviceNode(path, id string, node *yaml.Node) error {
	original, doc, err := readDocument(path)
	if err != nil {
		return err
	}
	devicesSeq, err := findDevicesSequence(doc)
	if err != nil {
		return err
	}
	for _, item := range devicesSeq.Content {
		if deviceNodeID(item) == id {
			return fmt.Errorf("device %q already exists", id)
		}
	}
	devicesSeq.Content = append(devicesSeq.Content, node)
	return writeValidated(path, original, doc)
}

// RemoveDevice deletes the device with the given id from the YAML file at
// path, using the same Node-level surgery as AddDevice. Returns an error
// wrapping ErrDeviceNotFound if no such device exists, so a caller like
// DeregisterDevice (registration.go) - and, through it,
// internal/apigateway's DeviceAdminService.RemoveDevice - can distinguish
// this from an operational failure with errors.Is instead of parsing text.
func RemoveDevice(path, id string) error {
	original, doc, err := readDocument(path)
	if err != nil {
		return err
	}
	devicesSeq, err := findDevicesSequence(doc)
	if err != nil {
		return err
	}

	kept := devicesSeq.Content[:0]
	found := false
	for _, item := range devicesSeq.Content {
		if deviceNodeID(item) == id {
			found = true
			continue
		}
		kept = append(kept, item)
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
	}
	devicesSeq.Content = kept

	return writeValidated(path, original, doc)
}

// SetDeviceEnabled flips the enabled field of the device with the given id,
// using the same Node-level surgery as AddDevice/RemoveDevice - every other
// field, and any comments/formatting elsewhere in the file, are untouched.
// Returns the updated device (decoded straight from the mutated node, no
// re-read of the file) so callers like RegisterDevice's counterpart in
// registration.go don't need a second config.Load. Returns an error if no
// such device exists.
func SetDeviceEnabled(path, id string, enabled bool) (config.Device, error) {
	original, doc, err := readDocument(path)
	if err != nil {
		return config.Device{}, err
	}
	devicesSeq, err := findDevicesSequence(doc)
	if err != nil {
		return config.Device{}, err
	}

	var target *yaml.Node
	for _, item := range devicesSeq.Content {
		if deviceNodeID(item) == id {
			target = item
			break
		}
	}
	if target == nil {
		return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
	}
	if err := setEnabledField(target, enabled); err != nil {
		return config.Device{}, err
	}

	if err := writeValidated(path, original, doc); err != nil {
		return config.Device{}, err
	}

	var device config.Device
	if err := target.Decode(&device); err != nil {
		return config.Device{}, fmt.Errorf("decode updated device: %w", err)
	}
	return device, nil
}

func setEnabledField(deviceNode *yaml.Node, enabled bool) error {
	for i := 0; i+1 < len(deviceNode.Content); i += 2 {
		if deviceNode.Content[i].Value == "enabled" {
			deviceNode.Content[i+1].Value = strconv.FormatBool(enabled)
			deviceNode.Content[i+1].Tag = "!!bool"
			return nil
		}
	}
	return errors.New("device has no enabled field")
}

func readDocument(path string) ([]byte, *yaml.Node, error) {
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return original, &doc, nil
}

func findDevicesSequence(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("unexpected YAML document shape")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("configuration root must be a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "devices" {
			seq := root.Content[i+1]
			if seq.Kind != yaml.SequenceNode {
				return nil, errors.New("devices must be a YAML sequence")
			}
			return seq, nil
		}
	}
	return nil, errors.New("configuration has no devices key")
}

func deviceNodeID(item *yaml.Node) string {
	if item.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "id" {
			return item.Content[i+1].Value
		}
	}
	return ""
}

// buildDevice constructs the config.Device value itself - no YAML involved
// yet. buildDeviceNode below marshals it into a *yaml.Node ready to splice
// into the document; AddDeviceFromTemplate also returns this value
// directly to its caller, so nothing needs to re-read the file to learn
// what was just created.
func buildDevice(id, deviceType, profile string, topicSuffixes []string) config.Device {
	enabled := true
	device := config.Device{
		ID:      id,
		Type:    deviceType,
		Profile: profile,
		Enabled: &enabled,
	}
	for _, suffix := range topicSuffixes {
		topic := "devices/" + id + "/"
		switch suffix {
		case "telemetry":
			device.Topics.Telemetry = topic + "telemetry"
		case "state":
			device.Topics.State = topic + "state"
		case "event":
			device.Topics.Event = topic + "event"
		case "command":
			device.Topics.Command = topic + "command"
		case "command_result":
			device.Topics.CommandResult = topic + "command-result"
		}
	}
	return device
}

// buildDeviceNode marshals a config.Device fragment (to get field order and
// omitempty behavior for free from the real type - see config.Topics' own
// comment) and re-parses just that fragment into a Node, ready to splice
// into the parent document's devices sequence. deviceType and profile are
// written as-is (profile empty means the omitempty tag drops it entirely,
// same as a device with no profile written by hand). Also returns the
// config.Device it built, so a caller that needs to report back what it
// just created doesn't have to re-read and re-parse the file.
func buildDeviceNode(id, deviceType, profile string, topicSuffixes []string) (*yaml.Node, config.Device, error) {
	device := buildDevice(id, deviceType, profile, topicSuffixes)

	fragment, err := yaml.Marshal(device)
	if err != nil {
		return nil, config.Device{}, fmt.Errorf("encode device fragment: %w", err)
	}
	var fragDoc yaml.Node
	if err := yaml.Unmarshal(fragment, &fragDoc); err != nil {
		return nil, config.Device{}, fmt.Errorf("re-parse device fragment: %w", err)
	}
	if fragDoc.Kind != yaml.DocumentNode || len(fragDoc.Content) != 1 {
		return nil, config.Device{}, errors.New("unexpected device fragment shape")
	}
	return fragDoc.Content[0], device, nil
}

func writeValidated(path string, original []byte, doc *yaml.Node) error {
	// Captured before writing: the admin service runs as root
	// (deploy/iot-gateway-admin.service), but the gateway itself reads
	// this file as the unprivileged iot-gateway user
	// (deploy/iot-gateway.service) - it must stay owned/grouped the way
	// its install step set it up (deploy/README.md: root:iot-gateway,
	// 0640), or the gateway fails closed with a permission error on its
	// very next restart. os.Stat failing here (path not found, e.g. a
	// fresh install) just means there is nothing to restore afterwards.
	originalInfo, statErr := os.Stat(path)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	updated := buf.Bytes()

	if _, err := config.Parse(updated); err != nil {
		return fmt.Errorf("updated configuration is invalid: %w", err)
	}

	if err := os.WriteFile(path+".bak", original, 0o640); err != nil {
		return fmt.Errorf("write backup %s.bak: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	// Best-effort, same principle deploy/mosquitto-provision-device.sh
	// already applies to the Mosquitto files it writes: restore the
	// pre-write owner/group/mode rather than trust that whatever wrote
	// the new content preserved them. restoreOwnership is a no-op on
	// Windows dev machines (see devices_windows.go) and never fails the
	// overall operation - the write itself already succeeded either way.
	if statErr == nil {
		restoreOwnership(path, originalInfo)
	}
	return nil
}
