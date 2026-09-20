package admin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Provision runs deploy/mosquitto-provision-device.sh to create (or rotate)
// a device's Mosquitto credential and ACL, then extracts the generated
// password from its stdout. The script itself never gets reimplemented in
// Go here - it is already the tested, privileged bash logic (dry-run
// exercised while it was written) and this just wraps it.
//
// The returned password is exactly what the script prints for the
// operator to paste into a device's secrets.h - AddDevice/RemoveDevice
// callers must show it to the caller exactly once and never log or store
// it anywhere else.
func Provision(ctx context.Context, scriptPath, deviceID string, topics []string) (password string, err error) {
	args := append([]string{deviceID}, topics...)
	cmd := exec.CommandContext(ctx, scriptPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return "", fmt.Errorf("provisioning script failed: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
	}
	password = extractPassword(stdout.String())
	if password == "" {
		return "", fmt.Errorf("provisioning script did not print a generated password")
	}
	return password, nil
}

// Deprovision runs the script's --remove mode to revoke a device's
// credential and ACL block.
func Deprovision(ctx context.Context, scriptPath, deviceID string) error {
	cmd := exec.CommandContext(ctx, scriptPath, "--remove", deviceID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("removal script failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Matches the script's own trailer, e.g.: `  #define MQTT_PASSWORD "..."`.
var passwordLinePattern = regexp.MustCompile(`(?m)^\s*#define MQTT_PASSWORD "([^"]+)"\s*$`)

func extractPassword(output string) string {
	match := passwordLinePattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}
