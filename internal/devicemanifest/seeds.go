package devicemanifest

// DefaultDocuments are inserted once into a new registry database. They are
// Go constants only for bootstrap; after insertion SQLite is the mutable
// source of truth and the Web editor creates subsequent revisions.
var DefaultDocuments = []string{
	`{"schema_version":1,"id":"esp32-c3-led","display_name":"ESP32-C3 LED","provisioning":{"protocol":"http-nvs-v1","model":"esp32c3-led","required_protocol_version":1},"mqtt":{"topics":["state","command"]},"capabilities":{"commands":[{"type":"set_led","parameters":{"on":{"type":"boolean","required":true}}}],"events":[{"type":"led.state_changed","payload":{"on":{"type":"boolean","required":true}}}]}}`,
	`{"schema_version":1,"id":"cyd-monitor","display_name":"CYD Monitor","provisioning":{"protocol":"http-nvs-v1","model":"cyd-monitor","required_protocol_version":1},"mqtt":{"topics":["command"]},"capabilities":{"commands":[{"type":"render_system_status","parameters":{}}],"events":[]}}`,
	`{"schema_version":1,"id":"orangepi-monitor","display_name":"Orange Pi Monitor","provisioning":{"protocol":"existing-identity-v1","model":"orangepi-monitor","required_protocol_version":0},"mqtt":{"topics":["telemetry"]},"capabilities":{"commands":[],"events":[{"type":"system.status","payload":{}}]}}`,
}
