// Package admin implements the LAN-facing device registration UI/API. It
// runs as a separate, privileged process from the sandboxed gateway (see
// docs/decisions.md ADR-008) and is the only part of this codebase that
// writes gateway.yaml or touches the Mosquitto broker's own files.
package admin

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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

	deviceNode, err := buildDeviceNode(id, topicSuffixes)
	if err != nil {
		return err
	}
	devicesSeq.Content = append(devicesSeq.Content, deviceNode)

	return writeValidated(path, original, doc)
}

// RemoveDevice deletes the device with the given id from the YAML file at
// path, using the same Node-level surgery as AddDevice. Returns an error if
// no such device exists.
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
		return fmt.Errorf("device %q not found", id)
	}
	devicesSeq.Content = kept

	return writeValidated(path, original, doc)
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

// buildDeviceNode marshals a config.Device fragment (to get field order and
// omitempty behavior for free from the real type - see config.Topics' own
// comment) and re-parses just that fragment into a Node, ready to splice
// into the parent document's devices sequence.
func buildDeviceNode(id string, topicSuffixes []string) (*yaml.Node, error) {
	enabled := true
	device := config.Device{
		ID:      id,
		Type:    "esp32",
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

	fragment, err := yaml.Marshal(device)
	if err != nil {
		return nil, fmt.Errorf("encode device fragment: %w", err)
	}
	var fragDoc yaml.Node
	if err := yaml.Unmarshal(fragment, &fragDoc); err != nil {
		return nil, fmt.Errorf("re-parse device fragment: %w", err)
	}
	if fragDoc.Kind != yaml.DocumentNode || len(fragDoc.Content) != 1 {
		return nil, errors.New("unexpected device fragment shape")
	}
	return fragDoc.Content[0], nil
}

func writeValidated(path string, original []byte, doc *yaml.Node) error {
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
	return nil
}
