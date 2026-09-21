package deviceprofile

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidateAndCanonicalize_Valid(t *testing.T) {
	got, err := ValidateAndCanonicalize("led.v1", "set_led", []byte(`{"on":true}`))
	if err != nil {
		t.Fatalf("ValidateAndCanonicalize() error = %v", err)
	}
	if string(got) != `{"on":true}` {
		t.Errorf("got %s, want canonical {\"on\":true}", got)
	}
}

func TestValidateAndCanonicalize_UnknownField(t *testing.T) {
	_, err := ValidateAndCanonicalize("led.v1", "set_led", []byte(`{"on":true,"extra":1}`))
	if err == nil {
		t.Fatal("want error for unknown field, got nil")
	}
}

func TestValidateAndCanonicalize_WrongType(t *testing.T) {
	_, err := ValidateAndCanonicalize("led.v1", "set_led", []byte(`{"on":"sim"}`))
	if err == nil {
		t.Fatal("want error for wrong field type, got nil")
	}
}

func TestValidateAndCanonicalize_MissingField(t *testing.T) {
	_, err := ValidateAndCanonicalize("led.v1", "set_led", []byte(`{}`))
	if err == nil {
		t.Fatal("want error for missing required field 'on', got nil")
	}
}

func TestValidateAndCanonicalize_CaseVariationRejected(t *testing.T) {
	// The field is named "on" - a single lowercase word is identical in
	// snake_case and lowerCamelCase, so there is no alternate spelling
	// protojson should accept. Any different casing or a different name
	// must be rejected as an unknown field, never silently accepted.
	_, err := ValidateAndCanonicalize("led.v1", "set_led", []byte(`{"On":true}`))
	if err == nil {
		t.Fatal("want error for field name case variation 'On', got nil")
	}
	_, err = ValidateAndCanonicalize("led.v1", "set_led", []byte(`{"ledOn":true}`))
	if err == nil {
		t.Fatal("want error for unrelated field name 'ledOn', got nil")
	}
}

func TestValidateAndCanonicalize_UnknownCommandType(t *testing.T) {
	_, err := ValidateAndCanonicalize("led.v1", "toggle", []byte(`{"on":true}`))
	if err == nil {
		t.Fatal("want error for unknown command type, got nil")
	}
}

func TestValidateAndCanonicalize_UnknownProfile(t *testing.T) {
	_, err := ValidateAndCanonicalize("thermostat.v1", "set_led", []byte(`{"on":true}`))
	if err == nil {
		t.Fatal("want error for unknown profile, got nil")
	}
}

func TestExists(t *testing.T) {
	if !Exists("led.v1") {
		t.Error(`Exists("led.v1") = false, want true`)
	}
	if Exists("thermostat.v1") {
		t.Error(`Exists("thermostat.v1") = true, want false`)
	}
}

func TestDescribe(t *testing.T) {
	descriptors, err := Describe("led.v1")
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("len(descriptors) = %d, want 1", len(descriptors))
	}
	d := descriptors[0]
	if d.Type != "set_led" {
		t.Errorf("Type = %q, want set_led", d.Type)
	}
	if d.ParametersMessage != "iot.device.led.v1.SetLed" {
		t.Errorf("ParametersMessage = %q", d.ParametersMessage)
	}
	if d.ParametersSchema == nil || len(d.ParametersSchema.Field) != 1 {
		t.Fatalf("ParametersSchema = %#v", d.ParametersSchema)
	}
	field := d.ParametersSchema.Field[0]
	if field.GetName() != "on" || field.GetType() != descriptorpb.FieldDescriptorProto_TYPE_BOOL {
		t.Errorf("field = %#v", field)
	}
}

func TestDescribe_UnknownProfile(t *testing.T) {
	if _, err := Describe("thermostat.v1"); err == nil {
		t.Fatal("want error for unknown profile, got nil")
	}
}

func TestValidateRegistry_RealRegistryIsSelfContained(t *testing.T) {
	if err := validateRegistry(profiles); err != nil {
		t.Fatalf("validateRegistry(profiles) error = %v, want nil", err)
	}
}

// TestValidateSelfContained_RejectsExternalMessageReference builds a
// synthetic message descriptor whose only field references
// google.protobuf.Timestamp - a message declared OUTSIDE it - to verify
// the registry invariant actually rejects that shape. This is the
// violation the invariant exists to catch: without it, a command message
// could reference a type the client can't see from a single
// DescriptorProto (see the plan's "Invariante" note).
func TestValidateSelfContained_RejectsExternalMessageReference(t *testing.T) {
	fd := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("deviceprofile/test/bad.proto"),
		Package:    proto.String("deviceprofile.test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Bad"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("when"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".google.protobuf.Timestamp"),
					},
				},
			},
		},
	}
	file, err := protodesc.NewFile(fd, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile() error = %v", err)
	}
	md := file.Messages().Get(0)
	if err := validateSelfContained(md); err == nil {
		t.Fatal("validateSelfContained() = nil, want error for external message reference")
	} else if !strings.Contains(err.Error(), "Timestamp") {
		t.Errorf("error = %v, want it to name the external type", err)
	}
}

func TestValidateSelfContained_AllowsNestedMessage(t *testing.T) {
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("deviceprofile/test/good.proto"),
		Package: proto.String("deviceprofile.test2"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Good"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("inner"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".deviceprofile.test2.Good.Inner"),
					},
				},
				NestedType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Inner"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:   proto.String("value"),
								Number: proto.Int32(1),
								Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								Type:   descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
							},
						},
					},
				},
			},
		},
	}
	file, err := protodesc.NewFile(fd, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile() error = %v", err)
	}
	md := file.Messages().Get(0)
	if err := validateSelfContained(md); err != nil {
		t.Errorf("validateSelfContained() error = %v, want nil (Inner is nested)", err)
	}
}
