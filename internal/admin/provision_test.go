package admin

import "testing"

// Provision/Deprovision themselves just wrap exec.Command around
// deploy/mosquitto-provision-device.sh (already dry-run tested when it was
// written) - executing a real script is an integration concern for Linux
// CI/the real Orange Pi, not a portable Go unit test (Windows dev machines
// can't shebang-exec a .sh file the way exec.Command assumes). What's worth
// unit-testing here is the pure parsing of the script's own output.
func TestExtractPassword(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "real script trailer",
			output: `password file: /etc/mosquitto/passwd
acl file:      /etc/mosquitto/acl
mosquitto reloaded

Provisioned "esp32c3-led". Put this in the device's own (gitignored) secrets.h:

  #define MQTT_USERNAME "esp32c3-led"
  #define MQTT_PASSWORD "yKaGCkmEy8E5yLGThiBod/w04NI+cUU3"

Verify the grant (run from another terminal on the same network):
`,
			want: "yKaGCkmEy8E5yLGThiBod/w04NI+cUU3",
		},
		{name: "no password line", output: "removed credential for esp32c3-led\n", want: ""},
		{name: "empty output", output: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractPassword(test.output); got != test.want {
				t.Errorf("extractPassword() = %q, want %q", got, test.want)
			}
		})
	}
}
