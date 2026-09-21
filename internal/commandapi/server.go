// Package commandapi exposes a LAN-reachable HTTP endpoint that publishes a
// command to a device, so testing a device doesn't require SSH and a
// hand-built mosquitto_pub call. It is embedded inside the already-running
// `iot-gateway run` process and reuses its already-connected MQTT client
// (see docs/decisions.md) - unlike internal/diagnostics, which is
// deliberately read-only and loopback-only, this package is neither.
package commandapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CommandPublisher is the one capability this package needs from the
// gateway - satisfied structurally by *mqtt.Gateway (internal/mqtt) already,
// with no changes needed there. Declared here, not imported from
// internal/mqtt, so this package depends on a capability, not a concrete
// type.
type CommandPublisher interface {
	PublishCommand(ctx context.Context, deviceID string, payload []byte) error
}

// Credentials gate every request behind HTTP Basic Auth, read from
// environment variables by cmd/gateway - never a flag (which would leak
// into `ps` output and shell history) and never logged.
type Credentials struct {
	Username string
	Password string
}

// Config is everything the command server needs to run.
type Config struct {
	Address        string
	Credentials    Credentials
	RequestTimeout time.Duration
}

// Server is the LAN-facing command-publish HTTP server.
type Server struct {
	cfg       Config
	publisher CommandPublisher
	logger    *slog.Logger
	http      *http.Server
}

func New(cfg Config, publisher CommandPublisher, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("commands address is required")
	}
	if publisher == nil {
		return nil, errors.New("commands publisher is required")
	}
	if cfg.Credentials.Username == "" || cfg.Credentials.Password == "" {
		return nil, errors.New("commands username and password are required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, publisher: publisher, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /commands", s.handlePublish)

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
		return fmt.Errorf("listen commands on %q: %w", s.cfg.Address, err)
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("commands server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

// Shutdown stops accepting command requests and waits for active requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown commands server: %w", err)
	}
	return nil
}

func basicAuth(creds Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		validUser := subtle.ConstantTimeCompare([]byte(username), []byte(creds.Username)) == 1
		validPass := subtle.ConstantTimeCompare([]byte(password), []byte(creds.Password)) == 1
		if !ok || !validUser || !validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="iot-gateway commands"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// commandRequest is the HTTP request body. Parameters is decoded as
// json.RawMessage and passed through opaquely into the outgoing MQTT
// envelope rather than re-marshaled, so it round-trips byte-for-byte into
// what internal/mqtt's validateCommand re-parses.
type commandRequest struct {
	DeviceID   string          `json:"device_id"`
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// commandEnvelope is the outgoing MQTT payload shape docs/mqtt.md and
// internal/mqtt's validateCommand require: command_id (server-generated
// UUID), type, parameters (a JSON object).
type commandEnvelope struct {
	CommandID  string          `json:"command_id"`
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	commandType := strings.TrimSpace(req.Type)
	if deviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id is required"})
		return
	}
	if commandType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type is required"})
		return
	}
	if len(req.Parameters) == 0 {
		req.Parameters = json.RawMessage("{}")
	}

	commandID := uuid.NewString()
	payload, err := json.Marshal(commandEnvelope{CommandID: commandID, Type: commandType, Parameters: req.Parameters})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parameters must be a JSON object"})
		return
	}

	// Every rejection PublishCommand can return (unknown device, disabled
	// device, missing command topic, bad payload shape) is something the
	// caller can fix by changing the request - a single trusted operator,
	// not a public API, so collapsing them all to 400 for this MVP is a
	// deliberate simplification (see the plan's Context section) rather
	// than plumbing sentinel errors through internal/mqtt for a finer
	// 4xx/5xx split.
	if err := s.publisher.PublishCommand(ctx, deviceID, payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"command_id": commandID,
		"device_id":  deviceID,
		"status":     "published",
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
