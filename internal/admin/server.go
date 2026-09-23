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
	// CYD and LED use the same in-band first-boot provisioning protocol. Their
	// client and broker endpoint are deployment inputs, not YAML or SQLite
	// state. DeviceBrokerHost must be LAN-reachable from the ESP (not 127.0.0.1).
	CYD              cydprovision.Client
	LED              cydprovision.Client
	DeviceBrokerHost string
	DeviceBrokerPort uint16
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
	return &Server{cfg: cfg}, nil
}

// ProvisionDevice, SetDeviceEnabled and RemoveDevice below are thin
// wrappers around registration.go's free functions, adding a request
// timeout (s.cfg.RequestTimeout) and supplying s.cfg.ConfigPath/
// ProvisionScript so a caller doesn't need to know either path. This is
// how *Server structurally satisfies internal/apigateway's DeviceAdmin
// interface - the exact same pattern *mqtt.Gateway already uses to satisfy
// internal/api's CommandPublisher/StatusProvider - so cmd/gateway/main.go
// can construct one and pass it straight into apigateway.New.

// ProvisionDevice registers a new device from template and returns its
// generated Mosquitto password for one-time display - see RegisterDevice's
// doc comment for the exact ordering and rollback semantics.
func (s *Server) ProvisionDevice(ctx context.Context, id, template string) (config.Device, string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry != nil {
		return s.provisionRegistry(ctx, id, template)
	}
	return RegisterDevice(ctx, s.cfg.ConfigPath, s.cfg.ProvisionScript, id, template)
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
	device := buildDevice(id, tmpl.Type, tmpl.Profile, tmpl.Topics)
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

// ProvisionCYD supplies a new, Wi-Fi-connected CYD with its broker identity
// over its one-time local HTTP endpoint. The password is kept in memory only:
// it is sent directly to the CYD and never returned to gateway-web.
func (s *Server) ProvisionCYD(ctx context.Context, id, address string) (config.Device, string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return config.Device{}, "", errors.New("CYD provisioning requires the SQLite registry")
	}
	if s.cfg.CYD == nil || strings.TrimSpace(s.cfg.DeviceBrokerHost) == "" || s.cfg.DeviceBrokerPort == 0 {
		return config.Device{}, "", errors.New("CYD provisioning is not configured")
	}
	tmpl, ok := deviceTemplates["cyd_monitor.v1"]
	if !ok {
		return config.Device{}, "", errors.New("cyd_monitor.v1 template is missing")
	}
	if err := config.ValidateDeviceID("device_id", id); err != nil {
		return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceID, err)
	}
	if _, err := s.cfg.CYD.Inspect(ctx, address); err != nil {
		if errors.Is(err, cydprovision.ErrInvalidIPAddress) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceAddress, err)
		}
		if errors.Is(err, cydprovision.ErrUnexpectedDevice) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrDeviceNotProvisionable, err)
		}
		return config.Device{}, "", fmt.Errorf("inspect CYD: %w", err)
	}

	// The registry intentionally starts disabled. If delivery is interrupted
	// after NVS was written, the operator can safely use SetDeviceEnabled to
	// finish activation without rotating a password the CYD already holds.
	device := buildDevice(id, tmpl.Type, tmpl.Profile, tmpl.Topics)
	disabled := false
	device.Enabled = &disabled
	if _, err := s.cfg.Registry.AddDevice(ctx, device, uuid.NewString()); err != nil {
		if errors.Is(err, registry.ErrDeviceAlreadyExists) {
			return config.Device{}, "", fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
		}
		return config.Device{}, "", fmt.Errorf("add CYD to registry: %w", err)
	}
	cleanupBeforeDelivery := func(cause error) (config.Device, string, error) {
		if revokeErr := s.cfg.Credentials.Revoke(ctx, id); revokeErr != nil {
			return config.Device{}, "", fmt.Errorf("%v; revoke CYD credential also failed: %w", cause, revokeErr)
		}
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			return config.Device{}, "", fmt.Errorf("%v; remove pending CYD registry entry also failed: %w", cause, removeErr)
		}
		return config.Device{}, "", cause
	}

	password, err := s.cfg.Credentials.Provision(ctx, id, tmpl.Topics)
	if err != nil {
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			return config.Device{}, "", fmt.Errorf("create CYD credential: %v; remove pending registry entry also failed: %w", err, removeErr)
		}
		return config.Device{}, "", fmt.Errorf("create CYD credential: %w", err)
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, false); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("disable new CYD credential: %w", err))
	}
	settings := cydprovision.Settings{
		DeviceID: id, BrokerHost: s.cfg.DeviceBrokerHost, BrokerPort: s.cfg.DeviceBrokerPort,
		Username: id, Password: password,
	}
	if err := s.cfg.CYD.Provision(ctx, address, settings); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("deliver CYD configuration: %w", err))
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, true); err != nil {
		return config.Device{}, "", fmt.Errorf("CYD stored its configuration but its broker credential remains disabled: %w", err)
	}
	active, err := s.cfg.Registry.SetDeviceEnabled(ctx, id, true, uuid.NewString())
	if err != nil {
		// Keep the recovery state coherent: a configured but disabled CYD can
		// be activated later using SetDeviceEnabled, without resending secrets.
		_ = s.cfg.Credentials.SetEnabled(context.Background(), id, false)
		return config.Device{}, "", fmt.Errorf("CYD stored its configuration but registry activation failed: %w", err)
	}
	return active, address, nil
}

// ProvisionLED supplies a new, Wi-Fi-connected ESP32-C3 LED with an MQTT
// identity over its temporary first-boot endpoint. As with CYD provisioning,
// the password moves directly from DynSec to NVS and never enters SQLite or a
// browser response.
func (s *Server) ProvisionLED(ctx context.Context, id, address string) (config.Device, string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if s.cfg.Registry == nil {
		return config.Device{}, "", errors.New("LED provisioning requires the SQLite registry")
	}
	if s.cfg.LED == nil || strings.TrimSpace(s.cfg.DeviceBrokerHost) == "" || s.cfg.DeviceBrokerPort == 0 {
		return config.Device{}, "", errors.New("LED provisioning is not configured")
	}
	tmpl, ok := deviceTemplates["esp32_led.v1"]
	if !ok {
		return config.Device{}, "", errors.New("esp32_led.v1 template is missing")
	}
	if err := config.ValidateDeviceID("device_id", id); err != nil {
		return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceID, err)
	}
	if _, err := s.cfg.LED.Inspect(ctx, address); err != nil {
		if errors.Is(err, cydprovision.ErrInvalidIPAddress) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrInvalidDeviceAddress, err)
		}
		if errors.Is(err, cydprovision.ErrUnexpectedDevice) {
			return config.Device{}, "", fmt.Errorf("%w: %v", ErrDeviceNotProvisionable, err)
		}
		return config.Device{}, "", fmt.Errorf("inspect LED: %w", err)
	}

	device := buildDevice(id, tmpl.Type, tmpl.Profile, tmpl.Topics)
	disabled := false
	device.Enabled = &disabled
	if _, err := s.cfg.Registry.AddDevice(ctx, device, uuid.NewString()); err != nil {
		if errors.Is(err, registry.ErrDeviceAlreadyExists) {
			return config.Device{}, "", fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
		}
		return config.Device{}, "", fmt.Errorf("add LED to registry: %w", err)
	}
	cleanupBeforeDelivery := func(cause error) (config.Device, string, error) {
		if revokeErr := s.cfg.Credentials.Revoke(ctx, id); revokeErr != nil {
			return config.Device{}, "", fmt.Errorf("%v; revoke LED credential also failed: %w", cause, revokeErr)
		}
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			return config.Device{}, "", fmt.Errorf("%v; remove pending LED registry entry also failed: %w", cause, removeErr)
		}
		return config.Device{}, "", cause
	}
	password, err := s.cfg.Credentials.Provision(ctx, id, tmpl.Topics)
	if err != nil {
		if removeErr := s.cfg.Registry.RemoveDevice(ctx, id, uuid.NewString()); removeErr != nil {
			return config.Device{}, "", fmt.Errorf("create LED credential: %v; remove pending registry entry also failed: %w", err, removeErr)
		}
		return config.Device{}, "", fmt.Errorf("create LED credential: %w", err)
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, false); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("disable new LED credential: %w", err))
	}
	settings := cydprovision.Settings{DeviceID: id, BrokerHost: s.cfg.DeviceBrokerHost, BrokerPort: s.cfg.DeviceBrokerPort, Username: id, Password: password}
	if err := s.cfg.LED.Provision(ctx, address, settings); err != nil {
		return cleanupBeforeDelivery(fmt.Errorf("deliver LED configuration: %w", err))
	}
	if err := s.cfg.Credentials.SetEnabled(ctx, id, true); err != nil {
		return config.Device{}, "", fmt.Errorf("LED stored its configuration but its broker credential remains disabled: %w", err)
	}
	active, err := s.cfg.Registry.SetDeviceEnabled(ctx, id, true, uuid.NewString())
	if err != nil {
		_ = s.cfg.Credentials.SetEnabled(context.Background(), id, false)
		return config.Device{}, "", fmt.Errorf("LED stored its configuration but registry activation failed: %w", err)
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
			return err
		}
		return nil
	}
	return DeregisterDevice(ctx, s.cfg.ConfigPath, s.cfg.ProvisionScript, id)
}

func (s *Server) provisionRegistry(ctx context.Context, id, template string) (config.Device, string, error) {
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
	device := buildDevice(id, tmpl.Type, tmpl.Profile, tmpl.Topics)
	password, err := s.cfg.Credentials.Provision(ctx, id, tmpl.Topics)
	if err != nil {
		return config.Device{}, "", fmt.Errorf("provisioning failed: %w", err)
	}
	if _, err := s.cfg.Registry.AddDevice(ctx, device, uuid.NewString()); err != nil {
		if rollbackErr := s.cfg.Credentials.Revoke(ctx, id); rollbackErr != nil {
			return config.Device{}, "", fmt.Errorf("registry rejected device after Mosquitto provision (%v); rollback also failed (%v)", err, rollbackErr)
		}
		if errors.Is(err, registry.ErrDeviceAlreadyExists) {
			return config.Device{}, "", fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, id)
		}
		return config.Device{}, "", fmt.Errorf("add device to registry: %w", err)
	}
	return device, password, nil
}
