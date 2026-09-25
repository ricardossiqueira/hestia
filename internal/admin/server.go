package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/cydprovision"
	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// Config is everything the device-administration engine needs. No address
// or credentials here any more (see docs/decisions.md ADR-015) -
// internal/apigateway is the only thing that ever talks to a network
// client, using its own (IOT_GATEWAY_API_USERNAME/PASSWORD).
type Config struct {
	ConfigPath      string
	ProvisionScript string
	// Credentials is the broker identity store. When nil, ProvisionScript is
	// retained solely for the legacy Mosquitto password_file migration path.
	Credentials CredentialStore
	// Registry is the runtime source of truth. ConfigPath is retained only
	// for the temporary YAML migration fallback when Registry is nil.
	Registry *registry.Store
	// ProvisioningClient returns an HTTP client constrained to the model from
	// a published manifest. Keeping the factory injectable makes the generic
	// protocol transaction testable without a device on the LAN.
	// DeviceBrokerHost must be LAN-reachable from the ESP (not 127.0.0.1).
	ProvisioningClient func(model string) cydprovision.Client
	DeviceBrokerHost   string
	DeviceBrokerPort   uint16
	// RequestTimeout bounds how long a single provisioning request (which
	// shells out to a script and restarts a systemd unit) may run.
	RequestTimeout time.Duration
}

// Server is the device-administration engine: provisioning a device from a
// template, enabling/disabling it, and removing it. It has no HTTP server
// of its own - a JSON/HTML UI on port 8081 used to live here, retired once
// gateway-web reached parity with it (ADR-015). *Server exists purely to
// satisfy internal/apigateway.DeviceAdmin structurally, the same pattern
// *mqtt.Gateway already uses for internal/api's capabilities.
type Server struct {
	cfg Config
}

func New(cfg Config) (*Server, error) {
	if cfg.Registry == nil && strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("admin config path is required")
	}
	if cfg.Credentials == nil {
		if strings.TrimSpace(cfg.ProvisionScript) == "" {
			return nil, errors.New("admin credential store or provision script is required")
		}
		cfg.Credentials = scriptCredentialStore{path: cfg.ProvisionScript}
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.ProvisioningClient == nil {
		cfg.ProvisioningClient = func(model string) cydprovision.Client {
			return cydprovision.NewHTTPClientForModel(cfg.RequestTimeout, model)
		}
	}
	return &Server{cfg: cfg}, nil
}

// RegisterExistingDevice adds policy for a local service whose DynSec client
// already exists. It intentionally never reads, returns, changes or rotates a
// broker password, so adopting orangepi-monitor cannot interrupt collection.
func (s *Server) RegisterExistingDevice(ctx context.Context, id, template string) (config.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return config.Device{}, errors.New("registering an existing device requires the SQLite registry")
	}
	tmpl, ok := deviceTemplates[template]
	if !ok {
		return config.Device{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, template)
	}
	if !tmpl.AdoptExisting {
		return config.Device{}, fmt.Errorf("%w: %q", ErrTemplateRequiresAdopt, template)
	}
	if err := config.ValidateDeviceID("device_id", id); err != nil {
		return config.Device{}, fmt.Errorf("%w: %v", ErrInvalidDeviceID, err)
	}
	device := buildDevice(id, tmpl.Type, tmpl.Topics)
	if _, err := s.cfg.Registry.AddDevice(ctx, device, uuid.NewString()); err != nil {
		if errors.Is(err, registry.ErrDeviceAlreadyExists) {
			return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
		}
		return config.Device{}, fmt.Errorf("add existing device to registry: %w", err)
	}
	return device, nil
}

// ListRoutes returns the current durable local routing policy. The registry
// is the authority in normal deployments; YAML remains read-only migration
// fallback only.
func (s *Server) ListRoutes(ctx context.Context) ([]config.Route, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry != nil {
		snapshot, err := s.cfg.Registry.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return snapshot.Routes, nil
	}
	cfg, err := config.Load(s.cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	return cfg.Routes, nil
}

// ListPublishedDeviceManifests returns only immutable revisions available for
// provisioning. Text editing and publication are intentionally introduced
// separately, so an unfinished draft can never drive broker policy.
func (s *Server) ListPublishedDeviceManifests(ctx context.Context) ([]registry.DeviceManifest, error) {
	if s.cfg.Registry == nil {
		return nil, errors.New("listing device manifests requires the SQLite registry")
	}
	return s.cfg.Registry.ListPublishedManifests(ctx)
}

// GetPublishedDeviceManifest returns one published definition by its stable
// manifest ID.
func (s *Server) GetPublishedDeviceManifest(ctx context.Context, id string) (registry.DeviceManifest, error) {
	if s.cfg.Registry == nil {
		return registry.DeviceManifest{}, errors.New("reading a device manifest requires the SQLite registry")
	}
	return s.cfg.Registry.GetPublishedManifest(ctx, id)
}

// ListDeviceManifestBindings provides the provision-time revision for each
// manifest-managed device. It is an audit read only and never changes runtime
// policy or broker state.
func (s *Server) ListDeviceManifestBindings(ctx context.Context) ([]registry.DeviceManifestBinding, error) {
	if s.cfg.Registry == nil {
		return nil, errors.New("listing device manifest bindings requires the SQLite registry")
	}
	return s.cfg.Registry.ListDeviceManifestBindings(ctx)
}

func (s *Server) MigrateDeviceToManifest(ctx context.Context, deviceID, manifestID, actor string) (config.Device, error) {
	if s.cfg.Registry == nil {
		return config.Device{}, errors.New("migrating a device requires the SQLite registry")
	}
	return s.cfg.Registry.MigrateDeviceToManifest(ctx, deviceID, manifestID, actor)
}

func (s *Server) CreateDeviceManifestDraft(ctx context.Context, document, actor string) (registry.DeviceManifest, error) {
	if s.cfg.Registry == nil {
		return registry.DeviceManifest{}, errors.New("editing device manifests requires the SQLite registry")
	}
	return s.cfg.Registry.CreateDeviceManifestDraft(ctx, document, actor)
}

func (s *Server) CreateDeviceManifestRevisionDraft(ctx context.Context, id, document, actor string) (registry.DeviceManifest, error) {
	if s.cfg.Registry == nil {
		return registry.DeviceManifest{}, errors.New("editing device manifests requires the SQLite registry")
	}
	return s.cfg.Registry.CreateDeviceManifestRevisionDraft(ctx, id, document, actor)
}

func (s *Server) PublishDeviceManifest(ctx context.Context, id string, revision uint64, actor string) (registry.DeviceManifest, error) {
	if s.cfg.Registry == nil {
		return registry.DeviceManifest{}, errors.New("editing device manifests requires the SQLite registry")
	}
	return s.cfg.Registry.PublishDeviceManifest(ctx, id, revision, actor)
}

// CreateRoute records a route in SQLite. It deliberately has no broker or
// systemd side effect: the long-lived gateway sees the new revision itself.
func (s *Server) CreateRoute(ctx context.Context, route config.Route) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return errors.New("route administration requires the SQLite registry")
	}
	if err := s.cfg.Registry.AddRoute(ctx, route); err != nil {
		if errors.Is(err, registry.ErrRouteAlreadyExists) {
			return fmt.Errorf("%w: %s", ErrRouteAlreadyExists, route.ID)
		}
		return err
	}
	return nil
}

// RemoveRoute removes a route from SQLite. As with CreateRoute, no restart
// is needed because the running gateway watches policy revisions.
func (s *Server) RemoveRoute(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return errors.New("route administration requires the SQLite registry")
	}
	if err := s.cfg.Registry.RemoveRoute(ctx, id); err != nil {
		if errors.Is(err, registry.ErrRouteNotFound) {
			return fmt.Errorf("%w: %s", ErrRouteNotFound, id)
		}
		return err
	}
	return nil
}

// ListAutomationRules returns every Marco 5 automation rule. Text editing
// (Update) is intentionally not exposed yet - see docs/device-manifests.md;
// changing a rule today means removing and recreating it, the same model
// routes already use.
func (s *Server) ListAutomationRules(ctx context.Context) ([]registry.AutomationRule, error) {
	if s.cfg.Registry == nil {
		return nil, errors.New("listing automation rules requires the SQLite registry")
	}
	return s.cfg.Registry.ListAutomationRules(ctx)
}

func (s *Server) CreateAutomationRule(ctx context.Context, rule registry.AutomationRule) (registry.AutomationRule, error) {
	if s.cfg.Registry == nil {
		return registry.AutomationRule{}, errors.New("creating an automation rule requires the SQLite registry")
	}
	return s.cfg.Registry.CreateAutomationRule(ctx, rule)
}

func (s *Server) SetAutomationRuleEnabled(ctx context.Context, id string, enabled bool) (registry.AutomationRule, error) {
	if s.cfg.Registry == nil {
		return registry.AutomationRule{}, errors.New("editing an automation rule requires the SQLite registry")
	}
	return s.cfg.Registry.SetAutomationRuleEnabled(ctx, id, enabled)
}

func (s *Server) RemoveAutomationRule(ctx context.Context, id string) error {
	if s.cfg.Registry == nil {
		return errors.New("removing an automation rule requires the SQLite registry")
	}
	return s.cfg.Registry.RemoveAutomationRule(ctx, id)
}

// ListInconsistencies returns provisioning operations whose best-effort
// compensation itself failed - see registry.Store.RecordInconsistency's
// doc comment and recordInconsistency below. Everything provisioned and
// removed cleanly never shows up here; an empty list is the healthy state.
func (s *Server) ListInconsistencies(ctx context.Context) ([]registry.Inconsistency, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return nil, errors.New("inconsistency tracking requires the SQLite registry")
	}
	return s.cfg.Registry.ListInconsistencies(ctx)
}

// ResolveInconsistency marks an entry resolved once an operator has fixed
// (or independently confirmed no fix is needed for) the underlying
// registry/broker disagreement. It does not itself touch the registry or
// the broker.
func (s *Server) ResolveInconsistency(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return errors.New("inconsistency tracking requires the SQLite registry")
	}
	if err := s.cfg.Registry.ResolveInconsistency(ctx, id); err != nil {
		if errors.Is(err, registry.ErrInconsistencyNotFound) {
			return fmt.Errorf("%w: %s", ErrInconsistencyNotFound, id)
		}
		return err
	}
	return nil
}

// ProvisionDeviceByIP provisions any published http-nvs-v1 manifest. The
// manifest is the durable source of the expected model and allowed MQTT
// topics; no hardware-specific RPC or YAML entry is needed for a new family.
// As with the legacy ESP methods, the DynSec password exists only while it is
// delivered to the device and is never persisted or returned to a browser.
func (s *Server) ProvisionDeviceByIP(ctx context.Context, id, manifestID, address string) (config.Device, string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return config.Device{}, "", errors.New("IP provisioning requires the SQLite registry")
	}
	if strings.TrimSpace(s.cfg.DeviceBrokerHost) == "" || s.cfg.DeviceBrokerPort == 0 {
		return config.Device{}, "", errors.New("IP provisioning is not configured")
	}
	if err := config.ValidateDeviceID("device_id", id); err != nil {
		return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceID, err)
	}
	manifest, err := s.cfg.Registry.GetPublishedManifest(ctx, manifestID)
	if err != nil {
		return config.Device{}, "", err
	}
	document, _, err := devicemanifest.Parse(manifest.Document)
	if err != nil {
		return config.Device{}, "", fmt.Errorf("published manifest %q is invalid: %w", manifestID, err)
	}
	if document.Provisioning.Protocol != "http-nvs-v1" {
		return config.Device{}, "", fmt.Errorf("%w: manifest %q does not support IP provisioning", ErrDeviceNotProvisionable, manifestID)
	}
	client := s.cfg.ProvisioningClient(document.Provisioning.Model)
	if client == nil {
		return config.Device{}, "", errors.New("IP provisioning client is not configured")
	}
	info, err := client.Inspect(ctx, address)
	if err != nil {
		if errors.Is(err, cydprovision.ErrInvalidIPAddress) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceAddress, err)
		}
		if errors.Is(err, cydprovision.ErrUnexpectedDevice) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrDeviceNotProvisionable, err)
		}
		return config.Device{}, "", fmt.Errorf("inspect device: %w", err)
	}
	if info.ProtocolVersion < document.Provisioning.RequiredProtocolVersion ||
		strings.TrimSpace(info.DeviceUID) == "" || strings.TrimSpace(info.FirmwareVersion) == "" {
		return config.Device{}, "", fmt.Errorf("%w: device firmware does not satisfy manifest %q", ErrDeviceNotProvisionable, manifestID)
	}

	// The record and broker credential deliberately begin disabled. Once NVS
	// receives a password, a later enable operation is recovery-safe and does
	// not rotate the password that the device already holds.
	device := buildDevice(id, document.Provisioning.Model, document.MQTT.Topics)
	disabled := false
	device.Enabled = &disabled
	if _, err := s.cfg.Registry.AddDevice(ctx, device, uuid.NewString()); err != nil {
		if errors.Is(err, registry.ErrDeviceAlreadyExists) {
			return config.Device{}, "", fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
		}
		return config.Device{}, "", fmt.Errorf("add device to registry: %w", err)
	}
	if err := s.cfg.Registry.BindDeviceManifest(ctx, id, manifest.ID, manifest.Revision); err != nil {
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			s.recordInconsistency("provision_device_by_ip", id, fmt.Errorf("bind manifest: %w", err), fmt.Errorf("remove pending registry entry also failed: %w", removeErr))
			return config.Device{}, "", fmt.Errorf("bind manifest: %v; remove pending registry entry also failed: %w", err, removeErr)
		}
		return config.Device{}, "", fmt.Errorf("bind manifest: %w", err)
	}
	cleanupBeforeDelivery := func(cause error) (config.Device, string, error) {
		if revokeErr := s.cfg.Credentials.Revoke(ctx, id); revokeErr != nil {
			s.recordInconsistency("provision_device_by_ip", id, cause, fmt.Errorf("revoke device credential also failed: %w", revokeErr))
			return config.Device{}, "", fmt.Errorf("%v; revoke device credential also failed: %w", cause, revokeErr)
		}
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			s.recordInconsistency("provision_device_by_ip", id, cause, fmt.Errorf("remove pending registry entry also failed: %w", removeErr))
			return config.Device{}, "", fmt.Errorf("%v; remove pending registry entry also failed: %w", cause, removeErr)
		}
		return config.Device{}, "", cause
	}
	password, err := s.cfg.Credentials.Provision(ctx, id, document.MQTT.Topics)
	if err != nil {
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			s.recordInconsistency("provision_device_by_ip", id, fmt.Errorf("create device credential: %w", err), fmt.Errorf("remove pending registry entry also failed: %w", removeErr))
			return config.Device{}, "", fmt.Errorf("create device credential: %v; remove pending registry entry also failed: %w", err, removeErr)
		}
		return config.Device{}, "", fmt.Errorf("create device credential: %w", err)
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, false); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("disable new device credential: %w", err))
	}
	settings := cydprovision.Settings{DeviceID: id, BrokerHost: s.cfg.DeviceBrokerHost, BrokerPort: s.cfg.DeviceBrokerPort, Username: id, Password: password}
	if err := client.Provision(ctx, address, settings); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("deliver device configuration: %w", err))
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, true); err != nil {
		s.recordInconsistency("provision_device_by_ip", id, errors.New("device already stored its configuration"), fmt.Errorf("enable broker credential failed: %w", err))
		return config.Device{}, "", fmt.Errorf("device stored its configuration but its broker credential remains disabled: %w", err)
	}
	active, err := s.cfg.Registry.SetDeviceEnabled(ctx, id, true, uuid.NewString())
	if err != nil {
		if rollbackErr := s.cfg.Credentials.SetEnabled(context.Background(), id, false); rollbackErr != nil {
			s.recordInconsistency("provision_device_by_ip", id, fmt.Errorf("registry activation failed: %w", err), fmt.Errorf("disable credential rollback also failed: %w", rollbackErr))
		}
		return config.Device{}, "", fmt.Errorf("device stored its configuration but registry activation failed: %w", err)
	}
	return active, address, nil
}

// SetDeviceEnabled toggles a device's enabled field and restarts the
// sandboxed gateway so the change takes effect.
func (s *Server) SetDeviceEnabled(ctx context.Context, id string, enabled bool) (config.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry != nil {
		snapshot, err := s.cfg.Registry.Snapshot(ctx)
		if err != nil {
			return config.Device{}, err
		}
		var previous *bool
		for _, candidate := range snapshot.Devices {
			if candidate.ID == id {
				previous = candidate.Enabled
				break
			}
		}
		if previous == nil {
			return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
		}
		if err := s.cfg.Credentials.SetEnabled(ctx, id, enabled); err != nil {
			return config.Device{}, fmt.Errorf("set Mosquitto credential enabled state: %w", err)
		}
		device, err := s.cfg.Registry.SetDeviceEnabled(ctx, id, enabled, uuid.NewString())
		if err != nil && *previous != enabled {
			if rollbackErr := s.cfg.Credentials.SetEnabled(ctx, id, *previous); rollbackErr != nil {
				s.recordInconsistency("set_device_enabled", id, fmt.Errorf("update registry after changing Mosquitto credential: %w", err), fmt.Errorf("rollback credential state also failed: %w", rollbackErr))
				return config.Device{}, fmt.Errorf("update registry after changing Mosquitto credential (%v); rollback credential state also failed (%v)", err, rollbackErr)
			}
		}
		if errors.Is(err, registry.ErrDeviceNotFound) {
			return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
		}
		return device, err
	}
	device, err := SetDeviceEnabled(s.cfg.ConfigPath, id, enabled)
	if err != nil {
		return config.Device{}, err
	}
	if err := RestartGateway(ctx); err != nil {
		return device, fmt.Errorf(
			"device %q was updated but the gateway service failed to restart: %w. "+
				"Restart it by hand to apply the change", id, err)
	}
	return device, nil
}

// RemoveDevice revokes a device's credential and removes it from
// gateway.yaml - see DeregisterDevice's doc comment for why there is no
// rollback on a partial failure here.
func (s *Server) RemoveDevice(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry != nil {
		if err := s.cfg.Credentials.Revoke(ctx, id); err != nil {
			return fmt.Errorf("revoke Mosquitto credential for %q: %w", id, err)
		}
		if err := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); err != nil {
			if errors.Is(err, registry.ErrDeviceNotFound) {
				return fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
			}
			// ADR-014: no automatic recovery here on purpose (recreating a
			// credential for a removal the operator asked for is riskier
			// than the removal itself), so this always needs a human -
			// unlike the other call sites above where the compensation
			// itself might still succeed.
			s.recordInconsistency("remove_device", id, errors.New("Mosquitto credential already revoked"), fmt.Errorf("registry removal failed: %w", err))
			return err
		}
		return nil
	}
	return DeregisterDevice(ctx, s.cfg.ConfigPath, s.cfg.ProvisionScript, id)
}

// recordInconsistency durably notes that a best-effort compensation failed
// (see the doc comments above each call site), so ListInconsistencies
// surfaces it to an operator even if nobody reads the HTTP error response
// that also describes it. It always uses context.Background(), never a
// caller's request-scoped ctx: that context may already be near
// cancellation (it is the same one bounded by s.cfg.RequestTimeout), and
// this write must still happen - the same reasoning ProvisionDeviceByIP
// already applies to its own best-effort credential rollback.
// Best-effort itself: if the SQLite write fails there is nothing more
// useful to do here than let the original error, which the caller already
// has, reach the operator.
func (s *Server) recordInconsistency(kind, deviceID string, cause, compensationError error) {
	if s.cfg.Registry == nil {
		return
	}
	_ = s.cfg.Registry.RecordInconsistency(context.Background(), kind, deviceID, cause.Error(), compensationError.Error())
}
