// Package devicemanifest defines the safe, versioned document used to
// describe a device family. It deliberately describes data only: manifests
// cannot carry broker credentials, raw ACLs, scripts or executable code.
package devicemanifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const (
	SchemaVersion = 1
	maxDocument   = 32 << 10
	maxCapability = 64
)

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var capabilityIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// Document is the versioned JSON contract stored in the registry. Parameter
// and payload schemas remain JSON objects because they will be interpreted by
// the generic command/event layer introduced in a later milestone.
type Document struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	DisplayName   string       `json:"display_name"`
	Provisioning  Provisioning `json:"provisioning"`
	MQTT          MQTT         `json:"mqtt"`
	Capabilities  Capabilities `json:"capabilities"`
}

type Provisioning struct {
	Protocol                string `json:"protocol"`
	Model                   string `json:"model"`
	RequiredProtocolVersion uint32 `json:"required_protocol_version"`
}

type MQTT struct {
	Topics []string `json:"topics"`
}

type Capabilities struct {
	Commands []Command `json:"commands"`
	Events   []Event   `json:"events"`
}

type Command struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// ValidateCommandParameters verifies one command against the compact v1
// parameter-schema vocabulary used by manifests. An empty schema accepts any
// JSON object; a non-empty schema permits only its declared fields.
func ValidateCommandParameters(document Document, commandType string, parameters []byte) ([]byte, error) {
	var schema json.RawMessage
	for _, command := range document.Capabilities.Commands {
		if command.Type == commandType {
			schema = command.Parameters
			break
		}
	}
	if len(schema) == 0 {
		return nil, fmt.Errorf("command type %q is not declared by manifest", commandType)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(parameters, &values); err != nil || values == nil {
		return nil, errors.New("command parameters must be a JSON object")
	}
	var fields map[string]struct {
		Type     string `json:"type"`
		Required bool   `json:"required"`
	}
	if err := json.Unmarshal(schema, &fields); err != nil {
		return nil, errors.New("manifest command parameters are invalid")
	}
	if len(fields) == 0 {
		return compactJSONObject(parameters)
	}
	for name, definition := range fields {
		if definition.Type != "boolean" && definition.Type != "string" && definition.Type != "number" && definition.Type != "integer" {
			return nil, fmt.Errorf("manifest command parameter %q has unsupported type %q", name, definition.Type)
		}
		value, exists := values[name]
		if !exists {
			if definition.Required {
				return nil, fmt.Errorf("command parameter %q is required", name)
			}
			continue
		}
		var decoded any
		if json.Unmarshal(value, &decoded) != nil || !matchesParameterType(decoded, definition.Type) {
			return nil, fmt.Errorf("command parameter %q must be %s", name, definition.Type)
		}
	}
	for name := range values {
		if _, known := fields[name]; !known {
			return nil, fmt.Errorf("command parameter %q is not declared", name)
		}
	}
	return compactJSONObject(parameters)
}

func compactJSONObject(value []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func matchesParameterType(value any, kind string) bool {
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
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	default:
		return false
	}
}

// Parse validates one document and returns a compact canonical encoding. The
// canonical text is what goes into SQLite, so equivalent whitespace-only
// edits do not produce ambiguous persisted data.
func Parse(document string) (Document, string, error) {
	if len(document) == 0 || len(document) > maxDocument {
		return Document{}, "", fmt.Errorf("manifest document must be between 1 and %d bytes", maxDocument)
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var parsed Document
	if err := decoder.Decode(&parsed); err != nil {
		return Document{}, "", fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Document{}, "", errors.New("manifest must contain one JSON document")
		}
		return Document{}, "", fmt.Errorf("decode manifest: %w", err)
	}
	if err := Validate(parsed); err != nil {
		return Document{}, "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(document)); err != nil {
		return Document{}, "", fmt.Errorf("compact manifest: %w", err)
	}
	return parsed, compact.String(), nil
}

// Validate applies the intentionally small v1 vocabulary. Adding a field is
// a schema-version change, rather than silently accepting configuration the
// running gateway does not understand.
func Validate(value Document) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema_version %d", value.SchemaVersion)
	}
	if !identifier.MatchString(value.ID) {
		return errors.New("manifest id must contain lowercase letters, numbers, hyphen or underscore")
	}
	if strings.TrimSpace(value.DisplayName) == "" || len(value.DisplayName) > 120 {
		return errors.New("manifest display_name must be between 1 and 120 characters")
	}
	if strings.TrimSpace(value.Provisioning.Model) == "" || len(value.Provisioning.Model) > 120 {
		return errors.New("manifest provisioning.model is required")
	}
	switch value.Provisioning.Protocol {
	case "http-nvs-v1":
		if value.Provisioning.RequiredProtocolVersion == 0 {
			return errors.New("http-nvs-v1 requires provisioning.required_protocol_version")
		}
	case "existing-identity-v1":
		if value.Provisioning.RequiredProtocolVersion != 0 {
			return errors.New("existing-identity-v1 must not require a device protocol version")
		}
	default:
		return errors.New("manifest provisioning.protocol is unsupported")
	}
	if len(value.MQTT.Topics) == 0 || len(value.MQTT.Topics) > 5 {
		return errors.New("manifest mqtt.topics must contain between 1 and 5 topics")
	}
	allowedTopics := map[string]bool{"telemetry": true, "state": true, "event": true, "command": true, "command-result": true}
	seenTopics := make(map[string]struct{}, len(value.MQTT.Topics))
	for _, topic := range value.MQTT.Topics {
		if !allowedTopics[topic] {
			return fmt.Errorf("manifest mqtt topic %q is unsupported", topic)
		}
		if _, exists := seenTopics[topic]; exists {
			return fmt.Errorf("manifest mqtt topic %q is duplicated", topic)
		}
		seenTopics[topic] = struct{}{}
	}
	if len(value.Capabilities.Commands) > maxCapability || len(value.Capabilities.Events) > maxCapability {
		return fmt.Errorf("manifest cannot define more than %d commands or events", maxCapability)
	}
	if err := validateCommandSchemas(value.Capabilities.Commands); err != nil {
		return err
	}
	return validateEventSchemas(value.Capabilities.Events)
}

func validateCommandSchemas(items []Command) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !capabilityIdentifier.MatchString(item.Type) {
			return fmt.Errorf("command type %q is invalid", item.Type)
		}
		if _, exists := seen[item.Type]; exists {
			return fmt.Errorf("command type %q is duplicated", item.Type)
		}
		seen[item.Type] = struct{}{}
		if err := validateJSONObject(item.Parameters, "command parameters"); err != nil {
			return err
		}
	}
	return nil
}

func validateEventSchemas(items []Event) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !capabilityIdentifier.MatchString(item.Type) {
			return fmt.Errorf("event type %q is invalid", item.Type)
		}
		if _, exists := seen[item.Type]; exists {
			return fmt.Errorf("event type %q is duplicated", item.Type)
		}
		seen[item.Type] = struct{}{}
		if err := validateJSONObject(item.Payload, "event payload"); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONObject(value json.RawMessage, label string) error {
	if len(value) == 0 || len(value) > 8<<10 {
		return fmt.Errorf("%s must be a JSON object up to 8192 bytes", label)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	return nil
}

// SortedTopics is useful to callers that compare a manifest policy without
// treating presentation order as a security-relevant difference.
func SortedTopics(value Document) []string {
	topics := append([]string(nil), value.MQTT.Topics...)
	sort.Strings(topics)
	return topics
}
