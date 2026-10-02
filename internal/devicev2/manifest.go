// Package devicev2 implements the device interface contract introduced by
// DEVICE_PLATFORM_V2_IMPLEMENTATION.md.  It is deliberately independent from
// the legacy manifest package: no v1 document can accidentally acquire v2
// MQTT permissions.
package devicev2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const SchemaVersion = 2

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// Manifest is the complete device-family interface. Direction is always from
// the device's perspective: Publish grants it broker write access; Subscribe
// grants it broker read access.
type Manifest struct {
	SchemaVersion   int    `json:"schema_version"`
	ManifestID      string `json:"manifest_id"`
	DisplayName     string `json:"display_name"`
	Model           string `json:"model"`
	ProtocolVersion uint32 `json:"protocol_version"`
	MQTT            MQTT   `json:"mqtt"`
}

type MQTT struct {
	Publish   []Output `json:"publish"`
	Subscribe []Input  `json:"subscribe"`
}

type Output struct {
	Channel  string          `json:"channel"`
	Retained *bool           `json:"retained,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Events   []Event         `json:"events,omitempty"`
}

type Input struct {
	Channel  string    `json:"channel"`
	Commands []Command `json:"commands,omitempty"`
}

type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type Command struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

type Field struct {
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// Parse validates exactly one JSON document and returns a deterministic
// canonical JSON encoding plus its SHA-256 hash. encoding/json's map key
// ordering is stable and recursively sorts object keys; structs preserve the
// fixed field order above.
func Parse(raw string) (Manifest, string, string, error) {
	if len(raw) == 0 || len(raw) > 32<<10 {
		return Manifest{}, "", "", errors.New("manifest must be between 1 and 32768 bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value Manifest
	if err := decoder.Decode(&value); err != nil {
		return Manifest{}, "", "", fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, "", "", errors.New("manifest must contain exactly one JSON object")
	}
	if err := Validate(value); err != nil {
		return Manifest{}, "", "", err
	}
	if err := normalize(&value); err != nil {
		return Manifest{}, "", "", err
	}
	canonicalBytes, err := json.Marshal(value)
	if err != nil {
		return Manifest{}, "", "", fmt.Errorf("canonicalize manifest: %w", err)
	}
	sum := sha256.Sum256(canonicalBytes)
	return value, string(canonicalBytes), hex.EncodeToString(sum[:]), nil
}

// normalize removes the last source of JSON ordering ambiguity: schemas are
// RawMessage values, so encoding/json otherwise preserves their input bytes.
func normalize(value *Manifest) error {
	for outputIndex := range value.MQTT.Publish {
		output := &value.MQTT.Publish[outputIndex]
		var err error
		if len(output.Schema) != 0 {
			output.Schema, err = canonicalObject(output.Schema)
			if err != nil {
				return err
			}
		}
		for eventIndex := range output.Events {
			output.Events[eventIndex].Payload, err = canonicalObject(output.Events[eventIndex].Payload)
			if err != nil {
				return err
			}
		}
	}
	for inputIndex := range value.MQTT.Subscribe {
		for commandIndex := range value.MQTT.Subscribe[inputIndex].Commands {
			canonical, err := canonicalObject(value.MQTT.Subscribe[inputIndex].Commands[commandIndex].Parameters)
			if err != nil {
				return err
			}
			value.MQTT.Subscribe[inputIndex].Commands[commandIndex].Parameters = canonical
		}
	}
	// Arrays form part of the signed document. They are semantic sets in v2,
	// so their wire order must not change the hash.
	sort.Slice(value.MQTT.Publish, func(i, j int) bool { return value.MQTT.Publish[i].Channel < value.MQTT.Publish[j].Channel })
	for outputIndex := range value.MQTT.Publish {
		sort.Slice(value.MQTT.Publish[outputIndex].Events, func(i, j int) bool {
			return value.MQTT.Publish[outputIndex].Events[i].Type < value.MQTT.Publish[outputIndex].Events[j].Type
		})
	}
	sort.Slice(value.MQTT.Subscribe, func(i, j int) bool { return value.MQTT.Subscribe[i].Channel < value.MQTT.Subscribe[j].Channel })
	for inputIndex := range value.MQTT.Subscribe {
		sort.Slice(value.MQTT.Subscribe[inputIndex].Commands, func(i, j int) bool {
			return value.MQTT.Subscribe[inputIndex].Commands[i].Type < value.MQTT.Subscribe[inputIndex].Commands[j].Type
		})
	}
	return nil
}

func canonicalObject(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func Validate(value Manifest) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if !identifier.MatchString(value.ManifestID) {
		return errors.New("manifest_id is invalid")
	}
	if strings.TrimSpace(value.DisplayName) == "" || len(value.DisplayName) > 120 {
		return errors.New("display_name must be between 1 and 120 characters")
	}
	if !identifier.MatchString(value.Model) {
		return errors.New("model is invalid")
	}
	if value.ProtocolVersion == 0 {
		return errors.New("protocol_version is required")
	}
	seen := map[string]bool{}
	for _, output := range value.MQTT.Publish {
		if !identifier.MatchString(output.Channel) {
			return fmt.Errorf("publish channel %q is invalid", output.Channel)
		}
		if seen[output.Channel] {
			return fmt.Errorf("mqtt channel %q is duplicated", output.Channel)
		}
		seen[output.Channel] = true
		switch output.Channel {
		case "telemetry", "command-result":
			if output.Retained != nil && *output.Retained {
				return fmt.Errorf("%s must not be retained", output.Channel)
			}
			if len(output.Events) != 0 {
				return fmt.Errorf("%s must use schema, not events", output.Channel)
			}
			if err := validateSchema(output.Schema, output.Channel+" schema"); err != nil {
				return err
			}
		case "state":
			if output.Retained == nil || !*output.Retained {
				return errors.New("state must declare retained: true")
			}
			if len(output.Events) != 0 {
				return errors.New("state must use schema, not events")
			}
			if err := validateSchema(output.Schema, "state schema"); err != nil {
				return err
			}
		case "event":
			if output.Retained != nil {
				return errors.New("event must not declare retained")
			}
			if len(output.Schema) != 0 {
				return errors.New("event must use events, not schema")
			}
			if len(output.Events) == 0 {
				return errors.New("event requires at least one event")
			}
			if err := validateEvents(output.Events); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported publish channel %q", output.Channel)
		}
	}
	for _, input := range value.MQTT.Subscribe {
		if !identifier.MatchString(input.Channel) {
			return fmt.Errorf("subscribe channel %q is invalid", input.Channel)
		}
		if seen[input.Channel] {
			return fmt.Errorf("mqtt channel %q is duplicated", input.Channel)
		}
		seen[input.Channel] = true
		if input.Channel != "command" {
			return fmt.Errorf("unsupported subscribe channel %q", input.Channel)
		}
		if len(input.Commands) == 0 {
			return errors.New("command requires at least one command")
		}
		if err := validateCommands(input.Commands); err != nil {
			return err
		}
	}
	return nil
}

func validateSchema(raw json.RawMessage, label string) error {
	if len(raw) == 0 {
		return fmt.Errorf("%s is required", label)
	}
	var fields map[string]Field
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return fmt.Errorf("%s must be an object", label)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one object", label)
	}
	for name, field := range fields {
		if !identifier.MatchString(name) {
			return fmt.Errorf("%s field %q is invalid", label, name)
		}
		switch field.Type {
		case "boolean", "string", "number", "integer":
		default:
			return fmt.Errorf("%s field %q has unsupported type %q", label, name, field.Type)
		}
	}
	return nil
}
func validateEvents(items []Event) error {
	seen := map[string]bool{}
	for _, item := range items {
		if !identifier.MatchString(item.Type) {
			return fmt.Errorf("event type %q is invalid", item.Type)
		}
		if seen[item.Type] {
			return fmt.Errorf("event type %q is duplicated", item.Type)
		}
		seen[item.Type] = true
		if err := validateSchema(item.Payload, "event payload"); err != nil {
			return err
		}
	}
	return nil
}
func validateCommands(items []Command) error {
	seen := map[string]bool{}
	for _, item := range items {
		if !identifier.MatchString(item.Type) {
			return fmt.Errorf("command type %q is invalid", item.Type)
		}
		if seen[item.Type] {
			return fmt.Errorf("command type %q is duplicated", item.Type)
		}
		seen[item.Type] = true
		if err := validateSchema(item.Parameters, "command parameters"); err != nil {
			return err
		}
	}
	return nil
}

func (m Manifest) Output(channel string) (Output, bool) {
	for _, item := range m.MQTT.Publish {
		if item.Channel == channel {
			return item, true
		}
	}
	return Output{}, false
}
func (m Manifest) Command(commandType string) (Command, bool) {
	for _, item := range m.MQTT.Subscribe {
		if item.Channel == "command" {
			for _, command := range item.Commands {
				if command.Type == commandType {
					return command, true
				}
			}
		}
	}
	return Command{}, false
}
func (m Manifest) Event(eventType string) (Event, bool) {
	if output, ok := m.Output("event"); ok {
		for _, event := range output.Events {
			if event.Type == eventType {
				return event, true
			}
		}
	}
	return Event{}, false
}

// Channels returns deterministic channel lists for policy presentation.
func (m Manifest) Channels() (publish, subscribe []string) {
	for _, item := range m.MQTT.Publish {
		publish = append(publish, item.Channel)
	}
	for _, item := range m.MQTT.Subscribe {
		subscribe = append(subscribe, item.Channel)
	}
	sort.Strings(publish)
	sort.Strings(subscribe)
	return
}

func ValidateFields(schema json.RawMessage, fields json.RawMessage) ([]byte, error) {
	var definitions map[string]Field
	var values map[string]json.RawMessage
	if err := json.Unmarshal(schema, &definitions); err != nil || definitions == nil {
		return nil, errors.New("invalid declared schema")
	}
	if err := json.Unmarshal(fields, &values); err != nil || values == nil {
		return nil, errors.New("fields must be a JSON object")
	}
	for name, definition := range definitions {
		value, ok := values[name]
		if !ok {
			if definition.Required {
				return nil, fmt.Errorf("field %q is required", name)
			}
			continue
		}
		if !matches(value, definition.Type) {
			return nil, fmt.Errorf("field %q must be %s", name, definition.Type)
		}
	}
	for name := range values {
		if _, ok := definitions[name]; !ok {
			return nil, fmt.Errorf("field %q is not declared", name)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, fields); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
func matches(raw json.RawMessage, kind string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch kind {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == float64(int64(n))
	}
	return false
}
