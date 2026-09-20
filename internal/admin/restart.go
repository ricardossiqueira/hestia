package admin

import (
	"context"
	"fmt"
	"os/exec"
)

// RestartGateway restarts the sandboxed iot-gateway.service so it picks up
// a gateway.yaml change. Only the admin service (running as root, unlike
// the gateway itself) can do this - see the package doc comment.
func RestartGateway(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemctl", "restart", "iot-gateway.service")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart iot-gateway.service: %w (%s)", err, string(output))
	}
	return nil
}
