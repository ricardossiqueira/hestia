// Package deviceprofile is the compiled registry of device command
// schemas. It is pure and does no I/O: every profile and command message
// is a Go value in this package, not something read from a file at
// runtime. internal/config imports it only to check that a configured
// profile name exists; internal/api imports it to validate and canonicalize
// PublishCommand parameters and to describe commands for
// ListDeviceCommands.
//
// A profile maps command "type" strings to Protobuf messages. Protobuf is
// the schema: ValidateAndCanonicalize rejects unknown fields, wrong field
// types, and any field declared "optional" in the .proto that is absent
// from the input (proto3 explicit presence - see
// api/proto/iot/device/led/v1/led.proto). MQTT itself is unaffected and
// stays JSON; only this validation layer is Protobuf-aware.
package deviceprofile

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	ledv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/device/led/v1"
)

// Profile is one compiled device profile: the set of command types it
// accepts, each backed by a Protobuf message that IS the schema for that
// command's "parameters".
type Profile struct {
	Commands map[string]proto.Message
}

// profiles is the compiled registry, deliberately a plain Go map literal
// so adding a device profile is a one-line, greppable change - not a file
// read, not reflection over a directory. "led.v1" is this etapa's only
// profile (see the plan's "Profiles nesta etapa" decision).
var profiles = map[string]Profile{
	"led.v1": {
		Commands: map[string]proto.Message{
			"set_led": (*ledv1.SetLed)(nil),
		},
	},
}

// init verifies the registry invariant documented on validateSelfContained
// for every command message in every profile, at process startup. A
// violation here is a programming error in this package (a maintainer
// added a profile whose command message references an external type), not
// a runtime/configuration error, so it panics rather than returning an
// error nothing would check.
func init() {
	if err := validateRegistry(profiles); err != nil {
		panic(fmt.Sprintf("internal/deviceprofile: invalid registry: %v", err))
	}
}

func validateRegistry(reg map[string]Profile) error {
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		profile := reg[name]
		types := make([]string, 0, len(profile.Commands))
		for t := range profile.Commands {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			md := profile.Commands[t].ProtoReflect().Descriptor()
			if err := validateSelfContained(md); err != nil {
				return fmt.Errorf("profile %q command %q: %w", name, t, err)
			}
		}
	}
	return nil
}

// Exists reports whether profile names a registry entry. internal/config's
// Config.Validate calls this so a typo in gateway.yaml's device.profile
// fails at `iot-gateway validate`, not silently at first PublishCommand.
func Exists(profile string) bool {
	_, ok := profiles[profile]
	return ok
}

// CommandDescriptor is what ListDeviceCommands sends a client so it can
// render a form without hardcoding any device's schema.
type CommandDescriptor struct {
	Type              string
	ParametersMessage string
	ParametersSchema  *descriptorpb.DescriptorProto
}

// Describe lists profile's commands, sorted by type.
func Describe(profile string) ([]CommandDescriptor, error) {
	p, ok := profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	types := make([]string, 0, len(p.Commands))
	for t := range p.Commands {
		types = append(types, t)
	}
	sort.Strings(types)

	descriptors := make([]CommandDescriptor, 0, len(types))
	for _, t := range types {
		md := p.Commands[t].ProtoReflect().Descriptor()
		descriptors = append(descriptors, CommandDescriptor{
			Type:              t,
			ParametersMessage: string(md.FullName()),
			ParametersSchema:  protodesc.ToDescriptorProto(md),
		})
	}
	return descriptors, nil
}

// ValidateAndCanonicalize validates params (a UTF-8 JSON object) against
// profile's schema for commandType and returns the canonical re-encoding
// that must be what actually reaches the device over MQTT.
//
// Two protojson behaviors do the schema enforcement:
//   - DiscardUnknown defaults to false, so a field not declared on the
//     message is a hard error (not silently dropped).
//   - Field types are enforced during unmarshal (e.g. a JSON string for a
//     bool field is an error), and field NAMES must match either the
//     proto field name or its lowerCamelCase form - nothing else.
//
// Presence is checked explicitly afterwards: every field declared
// `optional` in the .proto (proto3 explicit presence) must be present
// after unmarshal, via ProtoReflect().Has. Fields not declared optional
// are not presence-checked here - every command message in this registry
// must declare every field optional (see led.proto's comment) specifically
// so this check is meaningful for it.
//
// The canonical re-encoding uses UseProtoNames (snake_case, the exact
// wire/MQTT field name the firmware reads), so a client that sent
// lowerCamelCase input never leaks that spelling to the device.
func ValidateAndCanonicalize(profile, commandType string, params []byte) ([]byte, error) {
	p, ok := profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	prototype, ok := p.Commands[commandType]
	if !ok {
		return nil, fmt.Errorf("unknown command type %q for profile %q", commandType, profile)
	}

	// A fresh mutable instance of prototype's concrete type - prototype
	// itself may be a typed nil pointer (see the registry literal above),
	// so New() on its reflect.Message.Type() is what actually constructs
	// something protojson.Unmarshal can safely write into.
	msg := prototype.ProtoReflect().New().Interface()
	if err := (protojson.UnmarshalOptions{}).Unmarshal(params, msg); err != nil {
		return nil, fmt.Errorf("invalid parameters for %s/%s: %w", profile, commandType, err)
	}

	reflectMsg := msg.ProtoReflect()
	fields := reflectMsg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.HasOptionalKeyword() && !reflectMsg.Has(fd) {
			return nil, fmt.Errorf("field %q is required for %s/%s", fd.Name(), profile, commandType)
		}
	}

	canonical, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode canonical parameters: %w", err)
	}
	return canonical, nil
}

// validateSelfContained enforces the registry invariant: a command
// message may only contain scalar fields and message/enum fields whose
// type is declared NESTED inside that same top-level message (checked
// recursively). This is what lets Describe hand a client a single
// DescriptorProto - api.proto's CommandDescriptor.parameters_schema - and
// have it be the complete schema: nothing it references lives outside
// that one DescriptorProto tree.
func validateSelfContained(root protoreflect.MessageDescriptor) error {
	return checkFieldsSelfContained(root, root)
}

func checkFieldsSelfContained(root, md protoreflect.MessageDescriptor) error {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch fd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			nested := fd.Message()
			if !isNestedIn(root, nested.FullName()) {
				return fmt.Errorf("field %s.%s references external message %s; command messages must be self-contained", md.FullName(), fd.Name(), nested.FullName())
			}
			if err := checkFieldsSelfContained(root, nested); err != nil {
				return err
			}
		case protoreflect.EnumKind:
			en := fd.Enum()
			if !isNestedIn(root, en.FullName()) {
				return fmt.Errorf("field %s.%s references external enum %s; command messages must be self-contained", md.FullName(), fd.Name(), en.FullName())
			}
		}
	}
	return nil
}

func isNestedIn(root protoreflect.MessageDescriptor, name protoreflect.FullName) bool {
	return strings.HasPrefix(string(name), string(root.FullName())+".")
}
