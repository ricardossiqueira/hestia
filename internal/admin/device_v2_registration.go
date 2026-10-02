package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// V2CredentialIssuer is the only broker authority the v2 registration
// transaction needs. dynsec.Manager satisfies this interface directly.
type V2CredentialIssuer interface {
	ProvisionV2(context.Context, string, devicev2.Manifest) (string, error)
	SetEnabled(context.Context, string, bool) error
	Revoke(context.Context, string) error
}

// V2RegistrationCoordinator owns the privileged, ordered transaction from an
// inspected/pairable device to an active MQTT identity. It is deliberately
// independent from HTTP handlers: apigateway authenticates the operator and
// passes a discovery entry here; this package keeps credentials out of that
// public boundary.
type V2RegistrationCoordinator struct {
	Registry       *registry.Store
	Credentials    V2CredentialIssuer
	Provisioner    devicev2.SecureProvisioner
	MQTTBrokerHost string
	MQTTBrokerPort uint16
}

// Register completes the secure path:
//
//	manifest binding (pending) -> disabled DynSec credential -> encrypted
//	device provisioning -> enabled credential -> active registry binding.
//
// A failed delivery revokes the credential and marks the binding removed. The
// only non-compensated boundary is after the device confirms persistence: the
// device may hold a valid password while an enable/activation operation fails,
// so that state is kept visibly pending for explicit recovery instead of
// rotating the credential.
func (c V2RegistrationCoordinator) Register(ctx context.Context, entry devicev2.DiscoveryEntry, deviceID string) (registry.V2Device, error) {
	if c.Registry == nil || c.Credentials == nil || c.Provisioner == nil || strings.TrimSpace(c.MQTTBrokerHost) == "" || c.MQTTBrokerPort == 0 {
		return registry.V2Device{}, errors.New("v2 registration coordinator is not configured")
	}
	if entry.Info == nil || entry.State != devicev2.PairingRequired || !entry.Info.PairingRequired {
		return registry.V2Device{}, errors.New("device is not ready for secure pairing")
	}
	manifest, canonical, hash, err := devicev2.Parse(string(entry.Info.Manifest))
	if err != nil || hash != entry.ManifestSHA256 || entry.Info.DeviceUID != entry.DeviceUID || entry.Info.Model != entry.Model {
		return registry.V2Device{}, errors.New("discovery manifest is invalid")
	}
	accepted, err := c.Registry.AcceptV2Manifest(ctx, canonical, v2Fingerprint(entry.Info.IdentityPublicKey))
	if err != nil {
		return registry.V2Device{}, fmt.Errorf("accept device manifest: %w", err)
	}
	_, err = c.Registry.RegisterV2Device(ctx, registry.V2Device{
		DeviceID: deviceID, DeviceUID: entry.DeviceUID, ManifestID: manifest.ManifestID,
		ManifestRevision: accepted.Revision, ManifestSHA256: accepted.SHA256,
		FirmwareVersion: entry.Info.FirmwareVersion, IdentityPublicKey: entry.Info.IdentityPublicKey,
	})
	if err != nil {
		return registry.V2Device{}, fmt.Errorf("create pending device binding: %w", err)
	}

	password, err := c.Credentials.ProvisionV2(ctx, deviceID, manifest)
	if err != nil {
		_, _ = c.Registry.SetV2DeviceState(ctx, deviceID, "removed", "broker_provision_failed")
		return registry.V2Device{}, fmt.Errorf("create device credential: %w", err)
	}
	cleanup := func(cause error) (registry.V2Device, error) {
		_ = c.Credentials.SetEnabled(context.Background(), deviceID, false)
		if revokeErr := c.Credentials.Revoke(context.Background(), deviceID); revokeErr != nil {
			_, _ = c.Registry.SetV2DeviceState(context.Background(), deviceID, "pending", "credential_revoke_failed")
			return registry.V2Device{}, fmt.Errorf("%v; credential revoke also failed: %w", cause, revokeErr)
		}
		_, _ = c.Registry.SetV2DeviceState(context.Background(), deviceID, "removed", "provisioning_failed")
		return registry.V2Device{}, cause
	}
	if err := c.Credentials.SetEnabled(ctx, deviceID, false); err != nil {
		return cleanup(fmt.Errorf("disable new device credential: %w", err))
	}
	if err := c.Provisioner.PairAndProvision(ctx, entry.Announcement, *entry.Info, devicev2.ProvisionSettings{
		DeviceID: deviceID, ManifestSHA256: accepted.SHA256, MQTTHost: c.MQTTBrokerHost,
		MQTTPort: c.MQTTBrokerPort, MQTTUsername: deviceID, MQTTPassword: password,
	}); err != nil {
		return cleanup(fmt.Errorf("secure device provisioning: %w", err))
	}
	if err := c.Credentials.SetEnabled(ctx, deviceID, true); err != nil {
		_, _ = c.Registry.SetV2DeviceState(context.Background(), deviceID, "pending", "broker_enable_failed_after_device_persisted")
		return registry.V2Device{}, fmt.Errorf("device persisted configuration but broker enable failed: %w", err)
	}
	active, err := c.Registry.SetV2DeviceState(ctx, deviceID, "active", "active")
	if err != nil {
		_ = c.Credentials.SetEnabled(context.Background(), deviceID, false)
		return registry.V2Device{}, fmt.Errorf("device persisted configuration but registry activation failed: %w", err)
	}
	return active, nil
}

func v2Fingerprint(value string) string {
	// The immutable public key itself is retained on the binding. This short
	// marker is only catalog provenance, not an authorization decision.
	if len(value) > 120 {
		return value[:120]
	}
	return value
}
