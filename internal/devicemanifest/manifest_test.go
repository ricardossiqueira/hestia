package devicemanifest

import "testing"

func TestParseAcceptsAConstrainedManifest(t *testing.T) {
	document := `{
  "schema_version": 1,
  "id": "test-led",
  "display_name": "Test LED",
  "provisioning": {"protocol":"http-nvs-v1","model":"test-led","required_protocol_version":1},
  "mqtt": {"topics":["state","command"]},
  "capabilities": {
    "commands":[{"type":"set_led","parameters":{"on":{"type":"boolean"}}}],
    "events":[{"type":"led_state_changed","payload":{"on":{"type":"boolean"}}}]
  }
}`
	parsed, canonical, err := Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "test-led" || canonical == document || canonical == "" {
		t.Fatalf("Parse() = %#v, %q", parsed, canonical)
	}
}

func TestParseRejectsUnknownOrUnsafeManifestFields(t *testing.T) {
	for _, document := range []string{
		`{"schema_version":1,"id":"led","display_name":"LED","provisioning":{"protocol":"http-nvs-v1","model":"led","required_protocol_version":1},"mqtt":{"topics":["command"]},"capabilities":{"commands":[],"events":[]},"script":"rm -rf"}`,
		`{"schema_version":1,"id":"led","display_name":"LED","provisioning":{"protocol":"http-nvs-v1","model":"led","required_protocol_version":1},"mqtt":{"topics":["devices/+/command"]},"capabilities":{"commands":[],"events":[]}}`,
		`{"schema_version":1,"id":"led","display_name":"LED","provisioning":{"protocol":"http-nvs-v1","model":"led","required_protocol_version":1},"mqtt":{"topics":["command"]},"capabilities":{"commands":[{"type":"set_led","parameters":[]}],"events":[]}}`,
	} {
		if _, _, err := Parse(document); err == nil {
			t.Fatalf("Parse(%s) error = nil", document)
		}
	}
}
