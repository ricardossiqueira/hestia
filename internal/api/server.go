// Package api implements the Connect-RPC server that exposes device
// listing, command-schema discovery, command publishing and gateway status.
// Unlike internal/commandapi (which it replaced), parameters are validated
// against a device's internal/deviceprofile schema before being published
// to MQTT, and a client can discover what a device accepts instead of
// guessing.
//
// Since docs/decisions.md ADR-013, this server is loopback-only and does
// its own neither auth nor CORS: it is never reached directly from the LAN
// any more, only by internal/apigateway's reverse proxy running in the
// admin (root) process on the same host, which authenticates and applies
// CORS at the public edge before forwarding. Binding to a loopback address
// (config.validateAPI enforces this for api.internal_address) IS this
// server's trust boundary - see internal/apigateway's package doc for the
// other half of this split.
//
// Same New/Start/Shutdown shape as the rest of this project's HTTP
// servers, embedded inside the already-running `iot-gateway run` process
// so PublishCommand reuses the gateway's already-connected MQTT client
// (see ADR-009, unchanged from internal/commandapi).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
)

// CommandPublisher is the one capability this package needs from the
// gateway to publish a command - satisfied structurally by *mqtt.Gateway
// (internal/mqtt) already, with no changes needed there. Declared here, not
// imported from internal/mqtt, so this package depends on a capability, not
// a concrete type - same pattern as internal/commandapi.CommandPublisher.
type CommandPublisher interface {
	PublishCommand(ctx context.Context, deviceID string, payload []byte) error
}

// StatusProvider is the one capability this package needs to serve
// GetStatus - satisfied structurally by *mqtt.Gateway already.
type StatusProvider interface {
	Snapshot() mqtt.Snapshot
}

// TelemetryProvider is the one capability this package needs to serve
// GetDeviceTelemetry - satisfied structurally by *mqtt.Gateway already,
// same pattern as CommandPublisher/StatusProvider above.
type TelemetryProvider interface {
	LastTelemetry(deviceID string) ([]byte, time.Time, bool)
}

type DeviceProvider interface {
	Devices() []config.Device
}

// Config is everything the API server needs to run.
type Config struct {
	// Address is api.internal_address - a loopback address only
	// internal/apigateway's reverse proxy (same host) ever connects to.
	Address        string
	RequestTimeout time.Duration
	// Registry is the already loaded and validated gateway configuration.
	// It is read by value here (never re-read from disk), preserving the
	// sandboxed gateway process's read-only posture.
	Registry config.Config
	// DeviceProvider supersedes Registry.Devices for live reads after a
	// registry revision is applied.
	DeviceProvider DeviceProvider
}

// Server is the loopback-only Connect-RPC server (gRPC, gRPC-Web and
// HTTP/JSON on one port).
type Server struct {
	cfg       Config
	publisher CommandPublisher
	status    StatusProvider
	telemetry TelemetryProvider
	logger    *slog.Logger
	http      *http.Server

	devices        map[string]config.Device
	deviceList     []config.Device
	deviceProvider DeviceProvider
}

func New(cfg Config, publisher CommandPublisher, status StatusProvider, telemetry TelemetryProvider, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("api address is required")
	}
	if publisher == nil {
		return nil, errors.New("api publisher is required")
	}
	if status == nil {
		return nil, errors.New("api status provider is required")
	}
	if telemetry == nil {
		return nil, errors.New("api telemetry provider is required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	deviceList := make([]config.Device, len(cfg.Registry.Devices))
	copy(deviceList, cfg.Registry.Devices)
	sort.Slice(deviceList, func(i, j int) bool { return deviceList[i].ID < deviceList[j].ID })
	devices := make(map[string]config.Device, len(deviceList))
	for _, d := range deviceList {
		devices[d.ID] = d
	}

	s := &Server{
		cfg:            cfg,
		publisher:      publisher,
		status:         status,
		telemetry:      telemetry,
		logger:         logger,
		devices:        devices,
		deviceList:     deviceList,
		deviceProvider: cfg.DeviceProvider,
	}

	mux := http.NewServeMux()
	devicePath, deviceHandler := apiv1connect.NewDeviceServiceHandler(s)
	mux.Handle(devicePath, deviceHandler)
	gatewayPath, gatewayHandler := apiv1connect.NewGatewayServiceHandler(s)
	mux.Handle(gatewayPath, gatewayHandler)

	s.http = &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Start begins serving in the background after binding the configured
// address. Binding failures are reported synchronously to the caller.
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.cfg.Address)
	if err != nil {
		return fmt.Errorf("listen api on %q: %w", s.cfg.Address, err)
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("api server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

// Shutdown stops accepting API requests and waits for active requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown api server: %w", err)
	}
	return nil
}

func (s *Server) deviceByID(id string) (config.Device, bool) {
	if s.deviceProvider != nil {
		for _, device := range s.deviceProvider.Devices() {
			if device.ID == id {
				return device, true
			}
		}
		return config.Device{}, false
	}
	d, ok := s.devices[id]
	return d, ok
}

func (s *Server) currentDevices() []config.Device {
	if s.deviceProvider != nil {
		return s.deviceProvider.Devices()
	}
	devices := make([]config.Device, len(s.deviceList))
	copy(devices, s.deviceList)
	return devices
}
