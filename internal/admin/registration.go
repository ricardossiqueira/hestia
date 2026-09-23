package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

// Sentinel errors RegisterDevice and DeregisterDevice's Server-method
// callers (server.go) return so internal/apigateway's DeviceAdminService
// can map them to distinct Connect codes (NotFound, AlreadyExists,
// InvalidArgument) instead of collapsing every failure to Internal. Wrapped
// with %w, so callers use errors.Is against these, never string matching.
var (
	ErrDeviceAlreadyExists    = errors.New("device already exists")
	ErrDeviceNotFound         = errors.New("device not found")
	ErrUnknownTemplate        = errors.New("unknown template")
	ErrInvalidDeviceID        = errors.New("invalid device id")
	ErrInvalidDeviceAddress   = errors.New("invalid device address")
	ErrDeviceNotProvisionable = errors.New("device is not ready for provisioning")
	ErrRouteAlreadyExists     = errors.New("route already exists")
	ErrRouteNotFound          = errors.New("route not found")
	ErrTemplateRequiresAdopt  = errors.New("template requires an existing broker identity")
	ErrInconsistencyNotFound  = errors.New("inconsistency not found")
)

// RegisterDevice provisions a Mosquitto credential/ACL for a new device
// from template, adds it to gateway.yaml, and restarts the sandboxed
// gateway - the atomic-with-rollback flow gateway-web/docs/spec.md section
// 8.1 requires for ProvisionDevice. Order mirrors the HTML flow's
// handleAddDevice in server.go (Mosquitto first, gateway.yaml second -
// same reasoning: if the gateway.yaml write fails, nothing has been added
// to the file the gateway actually reads, so the credential must not
// survive either).
//
// Rollback covers only the Mosquitto-then-YAML pair: if
// AddDeviceFromTemplate fails after Provision already succeeded,
// Deprovision runs automatically to undo the credential - an orphaned
// Mosquitto credential has no use and no way for an operator to discover
// it exists, unlike a device that is fully configured but not yet live.
// A RestartGateway failure AFTER the YAML write succeeds is deliberately
// NOT rolled back: gateway.yaml and Mosquitto already agree with each
// other, the device is fully configured, it just is not live until a
// restart happens - the exact same posture handleAddDevice already has for
// this step, unchanged.
func RegisterDevice(ctx context.Context, configPath, scriptPath, id, template string) (device config.Device, password string, err error) {
	tmpl, ok := deviceTemplates[template]
	if !ok {
		return config.Device{}, "", fmt.Errorf("%w: %q", ErrUnknownTemplate, template)
	}
	if tmpl.AdoptExisting {
		return config.Device{}, "", fmt.Errorf("%w: %q", ErrTemplateRequiresAdopt, template)
	}
	if err := config.ValidateDeviceID("device_id", id); err != nil {
		return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceID, err)
	}

	// Checked before provisioning, same reason handleAddDevice already
	// checks this first: Provision() ROTATES an existing device's
	// Mosquitto password as a side effect. Finding out about a YAML
	// conflict only after that already happened invalidates a working
	// device's credential with no way to recover the new password.
	exists, err := DeviceExists(configPath, id)
	if err != nil {
		return config.Device{}, "", err
	}
	if exists {
		return config.Device{}, "", fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
	}

	password, err = Provision(ctx, scriptPath, id, tmpl.Topics)
	if err != nil {
		return config.Device{}, "", fmt.Errorf("provisioning failed: %w", err)
	}

	device, err = AddDeviceFromTemplate(configPath, id, template)
	if err != nil {
		if rollbackErr := Deprovision(ctx, scriptPath, id); rollbackErr != nil {
			// A double failure: never hide the second one behind the
			// first - the credential is now orphaned and unrecoverable
			// automatically, so the operator needs both pieces of
			// information to fix it by hand.
			return config.Device{}, "", fmt.Errorf(
				"device %q was provisioned in Mosquitto but NOT added to gateway.yaml (%v); "+
					"automatic rollback ALSO failed, the credential is orphaned (%v). "+
					"Remove it by hand with the CLI script's --remove", id, err, rollbackErr)
		}
		return config.Device{}, "", fmt.Errorf(
			"device %q could not be added to gateway.yaml (Mosquitto credential was rolled back): %w", id, err)
	}

	if err := RestartGateway(ctx); err != nil {
		return device, password, fmt.Errorf(
			"device %q was registered but the gateway service failed to restart: %w. "+
				"Restart it by hand to apply the change", id, err)
	}
	return device, password, nil
}

// DeregisterDevice removes a device from gateway.yaml, revokes its
// Mosquitto credential/ACL, and restarts the sandboxed gateway - the exact
// sequence handleRemoveDevice (server.go) already performs for the HTML
// UI, extracted here so internal/apigateway's DeviceAdminService.RemoveDevice
// can reuse it instead of duplicating it. No rollback on a partial
// failure here, unlike RegisterDevice: recreating a device's registration
// and credential automatically after a failed removal is riskier than
// undoing a creation that just happened moments earlier in the same
// request, and the orphaned-but-still-valid credential left behind by a
// failed Deprovision is a bookkeeping problem, not a security exposure -
// same posture handleRemoveDevice already has.
func DeregisterDevice(ctx context.Context, configPath, scriptPath, id string) error {
	if err := RemoveDevice(configPath, id); err != nil {
		return fmt.Errorf("removing %q from gateway.yaml failed: %w", id, err)
	}
	if err := Deprovision(ctx, scriptPath, id); err != nil {
		return fmt.Errorf(
			"device %q was removed from gateway.yaml but its Mosquitto credential/ACL could not be revoked: %w. "+
				"Run the CLI script's --remove by hand.", id, err)
	}
	if err := RestartGateway(ctx); err != nil {
		return fmt.Errorf("device %q was removed but the gateway service failed to restart: %w", id, err)
	}
	return nil
}
