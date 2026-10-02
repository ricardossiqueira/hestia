package devicev2

import "testing"

const ledManifest = `{"schema_version":2,"manifest_id":"esp32-c3-led","display_name":"ESP32-C3 LED","model":"esp32c3-led","protocol_version":1,"mqtt":{"publish":[{"channel":"state","retained":true,"schema":{"online":{"type":"boolean","required":true},"on":{"type":"boolean","required":true}}},{"channel":"event","events":[{"type":"led.state_changed","payload":{"on":{"type":"boolean","required":true}}}]}],"subscribe":[{"channel":"command","commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}]}]}}`

func TestParseAndDeriveACL(t *testing.T) {
	manifest, canonical, hash, err := Parse(ledManifest)
	if err != nil {
		t.Fatal(err)
	}
	if canonical == "" || len(hash) != 64 {
		t.Fatalf("canonical/hash = %q/%q", canonical, hash)
	}
	acls, err := DeriveACL("led-sala", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(acls) != 3 || acls[0].Direction != "write" || acls[2].Direction != "read" {
		t.Fatalf("ACLs = %#v", acls)
	}
}
func TestParseRejectsWrongDirectionAndInvalidPayload(t *testing.T) {
	bad := `{"schema_version":2,"manifest_id":"x","display_name":"X","model":"x","protocol_version":1,"mqtt":{"publish":[{"channel":"command","schema":{}}],"subscribe":[]}}`
	if _, _, _, err := Parse(bad); err == nil {
		t.Fatal("expected invalid output")
	}
	manifest, _, _, err := Parse(ledManifest)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := manifest.Command("set_led")
	if !ok {
		t.Fatal("command missing")
	}
	if _, err := ValidateFields(command.Parameters, []byte(`{"on":"yes"}`)); err == nil {
		t.Fatal("expected schema violation")
	}
}

func TestCanonicalHashIgnoresCollectionAndSchemaOrder(t *testing.T) {
	first := `{"schema_version":2,"manifest_id":"ordered","display_name":"Ordered","model":"ordered","protocol_version":1,"mqtt":{"publish":[{"channel":"telemetry","schema":{"z":{"type":"string"},"a":{"type":"boolean"}}},{"channel":"event","events":[{"type":"z.event","payload":{}},{"type":"a.event","payload":{}}]}],"subscribe":[{"channel":"command","commands":[{"type":"z-command","parameters":{}},{"type":"a-command","parameters":{}}]}]}}`
	second := `{"model":"ordered","display_name":"Ordered","manifest_id":"ordered","schema_version":2,"protocol_version":1,"mqtt":{"subscribe":[{"commands":[{"parameters":{},"type":"a-command"},{"parameters":{},"type":"z-command"}],"channel":"command"}],"publish":[{"events":[{"payload":{},"type":"a.event"},{"payload":{},"type":"z.event"}],"channel":"event"},{"schema":{"a":{"type":"boolean"},"z":{"type":"string"}},"channel":"telemetry"}]}}`
	_, _, one, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	_, _, two, err := Parse(second)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("hashes differ: %s != %s", one, two)
	}
}

func TestParseRejectsUnknownFieldSchemaProperties(t *testing.T) {
	bad := `{"schema_version":2,"manifest_id":"bad","display_name":"Bad","model":"bad","protocol_version":1,"mqtt":{"publish":[{"channel":"telemetry","schema":{"x":{"type":"string","unsafe":true}}}],"subscribe":[]}}`
	if _, _, _, err := Parse(bad); err == nil {
		t.Fatal("unknown schema property was accepted")
	}
}
