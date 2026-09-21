// Package api implements the Connect-RPC server that exposes device
// listing, command-schema discovery, command publishing and gateway status
// to LAN clients (see docs/api-v1.md). It replaces internal/commandapi:
// unlike that package, parameters are validated against a device's
// internal/deviceprofile schema before being published to MQTT, and a
// client can discover what a device accepts instead of guessing.
//
// Same shape as internal/commandapi and internal/admin: New/Start/Shutdown
// around a *http.Server, HTTP Basic Auth via basicAuth (not a Connect
// interceptor, so an auth failure stays a plain 401 with WWW-Authenticate
// that curl -u and a browser understand), embedded inside the already-
// running `iot-gateway run` process so PublishCommand reuses the gateway's
// already-connected MQTT client (see docs/decisions.md ADR-009, which this
// package inherits unchanged from internal/commandapi).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
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

// Credentials gate every request behind HTTP Basic Auth, read from
// environment variables by cmd/gateway - never a flag (which would leak
// into `ps` output and shell history) and never logged.
type Credentials struct {
	Username string
	Password string
}

// Config is everything the API server needs to run.
type Config struct {
	Address        string
	Credentials    Credentials
	RequestTimeout time.Duration
	// Registry is the already loaded and validated gateway configuration.
	// It is read by value here (never re-read from disk), preserving the
	// sandboxed gateway process's read-only posture.
	Registry config.Config
}

// Server is the LAN-facing Connect-RPC server (gRPC, gRPC-Web and
// HTTP/JSON on one port).
type Server struct {
	cfg       Config
	publisher CommandPublisher
	status    StatusProvider
	logger    *slog.Logger
	http      *http.Server

	devices    map[string]config.Device
	deviceList []config.Device
}

func New(cfg Config, publisher CommandPublisher, status StatusProvider, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("api address is required")
	}
	if publisher == nil {
		return nil, errors.New("api publisher is required")
	}
	if status == nil {
		return nil, errors.New("api status provider is required")
	}
	if cfg.Credentials.Username == "" || cfg.Credentials.Password == "" {
		return nil, errors.New("api username and password are required")
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
		cfg:        cfg,
		publisher:  publisher,
		status:     status,
		logger:     logger,
		devices:    devices,
		deviceList: deviceList,
	}

	mux := http.NewServeMux()
	devicePath, deviceHandler := apiv1connect.NewDeviceServiceHandler(s)
	mux.Handle(devicePath, deviceHandler)
	gatewayPath, gatewayHandler := apiv1connect.NewGatewayServiceHandler(s)
	mux.Handle(gatewayPath, gatewayHandler)

	s.http = &http.Server{
		Addr:              cfg.Address,
		Handler:           basicAuth(cfg.Credentials, mux),
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

func basicAuth(creds Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		validUser := subtle.ConstantTimeCompare([]byte(username), []byte(creds.Username)) == 1
		validPass := subtle.ConstantTimeCompare([]byte(password), []byte(creds.Password)) == 1
		if !ok || !validUser || !validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="iot-gateway api"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) deviceByID(id string) (config.Device, bool) {
	d, ok := s.devices[id]
	return d, ok
}
