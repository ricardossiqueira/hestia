package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

// Config is everything the device-administration engine needs. No address
// or credentials here any more (see docs/decisions.md ADR-015) -
// internal/apigateway is the only thing that ever talks to a network
// client, using its own (IOT_GATEWAY_API_USERNAME/PASSWORD).
type Config struct {
	ConfigPath      string
	ProvisionScript string
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
	if strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("admin config path is required")
	}
	if strings.TrimSpace(cfg.ProvisionScript) == "" {
		return nil, errors.New("admin provision script path is required")
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
	return RegisterDevice(ctx, s.cfg.ConfigPath, s.cfg.ProvisionScript, id, template)
}

// SetDeviceEnabled toggles a device's enabled field and restarts the
// sandboxed gateway so the change takes effect.
func (s *Server) SetDeviceEnabled(ctx context.Context, id string, enabled bool) (config.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
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
	return DeregisterDevice(ctx, s.cfg.ConfigPath, s.cfg.ProvisionScript, id)
}
