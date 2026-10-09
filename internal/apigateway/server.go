// Package apigateway is the public edge of the Connect-RPC API
// (docs/api-v1.md), embedded inside `iot-gateway admin` (root - see
// internal/admin's package doc and docs/decisions.md ADR-008/ADR-013).
//
// It is the ONLY process that binds api.address, the LAN-reachable port
// Hera and any other client talk to. It authenticates browser sessions and
// legacy HTTP Basic clients, then applies CORS once at this edge. GetStatus
// aggregates registry and discovery here with runtime counters from
// internal/api; other GatewayService
// methods are reverse-proxied to the sandboxed `iot-gateway run`
// process and binds only api.internal_address, a loopback address
// unreachable from the LAN. V2's device platform (DeviceV2) is served
// directly at this edge too - see device_v2.go.
//
// This package used to also answer DeviceAdminService directly (V1's
// register/enable-disable/remove/routes/manifests/automations surface,
// delegated to internal/admin's *Server) - retired once the V2 device
// platform fully covered it; see docs/decisions.md's V1-removal ADR.
//
// Why the proxy split exists at all: internal/api needs the sandboxed
// process's live MQTT connection to publish a command (ADR-009), but admin
// operations need root to touch Mosquitto/systemd (ADR-008) - two
// capabilities that must never live in the same process. Two processes
// cannot bind the same port, so one of them has to be a proxy for the
// other; ADR-013 records why this one (root) is the one holding the public
// port, not the sandboxed one. It was already the LAN listener at the time
// (a JSON/HTML UI on :8081, retired since - ADR-015).
package apigateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/operatorauth"
)

// Credentials gate every request behind HTTP Basic Auth, read from
// environment variables by cmd/gateway (IOT_GATEWAY_API_USERNAME/
// PASSWORD) - never a flag (which would leak into `ps` output and shell
// history) and never logged.
type Credentials struct {
	Username string
	Password string
}

// Config is everything the public API edge needs to run.
type Config struct {
	// Address is api.address - the public, LAN-reachable listener.
	Address string
	// InternalAPIURL is where internal/api is listening, e.g.
	// "http://127.0.0.1:8083" (built from api.internal_address by the
	// caller). GatewayService requests other than the aggregated GetStatus
	// are reverse-proxied unchanged, Authorization included (internal/api ignores it -
	// loopback is its trust boundary, see its package doc - but there is no
	// reason to strip it either).
	InternalAPIURL string
	Credentials    Credentials
	// AllowedOrigins is api.cors_allowed_origins passed through unchanged.
	// Empty means CORS is off - see cors' doc comment.
	AllowedOrigins []string
	// DeviceV2 is the authenticated JSON RPC adapter for the device platform
	// v2.
	DeviceV2     *DeviceV2API
	OperatorAuth *operatorauth.Service
}

// Server is the public-facing Connect-RPC edge: auth, CORS, aggregated status,
// a runtime proxy and the V2 device platform.
type Server struct {
	cfg     Config
	logger  *slog.Logger
	http    *http.Server
	runtime apiv1connect.GatewayServiceClient
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("apigateway address is required")
	}
	if strings.TrimSpace(cfg.InternalAPIURL) == "" {
		return nil, errors.New("apigateway internal API URL is required")
	}
	target, err := url.Parse(cfg.InternalAPIURL)
	if err != nil {
		return nil, fmt.Errorf("apigateway internal API URL is invalid: %w", err)
	}
	if cfg.Credentials.Username == "" || cfg.Credentials.Password == "" {
		return nil, errors.New("apigateway username and password are required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, logger: logger}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.logger.Error("apigateway proxy to internal API failed", "error", err, "path", r.URL.Path)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "internal API unreachable"})
	}

	mux := http.NewServeMux()
	mux.Handle("/iot.gateway.api.v1.GatewayService/", proxy)
	if cfg.DeviceV2 != nil {
		s.runtime = apiv1connect.NewGatewayServiceClient(&http.Client{Timeout: 10 * time.Second}, cfg.InternalAPIURL)
		mux.Handle(apiv1connect.GatewayServiceGetStatusProcedure, connect.NewUnaryHandler(apiv1connect.GatewayServiceGetStatusProcedure, s.getStatus))
		mux.Handle("/iot.gateway.api.v2.DevicePlatformService/", cfg.DeviceV2)
	}

	// cors wraps basicAuth, not the reverse: a browser's CORS preflight
	// (OPTIONS) never carries the Authorization header being negotiated,
	// so it must be answered before auth ever runs.
	protected := http.Handler(basicAuth(cfg.Credentials, mux))
	if cfg.OperatorAuth != nil {
		protected = cfg.OperatorAuth.Handler(mux)
	}
	s.http = &http.Server{
		Addr:              cfg.Address,
		Handler:           cors(cfg.AllowedOrigins, protected),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Start begins serving in the background after binding the configured
// address. Binding failures are reported synchronously to the caller.
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.cfg.Address)
	if err != nil {
		return fmt.Errorf("listen apigateway on %q: %w", s.cfg.Address, err)
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("apigateway server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

// Shutdown stops accepting requests and waits for active ones.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown apigateway server: %w", err)
	}
	return nil
}

// corsAllowedHeaders and corsAllowedMethods cover Hera's JSON RPC calls and
// operator session endpoints. The origin allowlist remains exact, never '*'.
const (
	corsAllowedHeaders = "Content-Type, Authorization, X-CSRF-Token"
	corsAllowedMethods = "GET, POST, OPTIONS"
)

// cors implements an exact-origin allowlist with credentials, per
// gateway-web/docs/spec.md section 5: a browser SPA on a different origin
// (e.g. http://localhost:5173) needs Access-Control-Allow-Origin echoing
// its own origin (never "*" - browsers reject "*" for credentialed
// requests anyway, and config.validateOrigin already rejects it at
// `iot-gateway validate`) plus Access-Control-Allow-Credentials: true, and
// a preflight OPTIONS answered without requiring auth.
//
// allowedOrigins empty (the default - config.API.AllowedOrigins' doc
// comment) makes this a no-op passthrough: no Access-Control-* header is
// ever added, so a non-browser caller (curl, a server) is unaffected.
func cors(allowedOrigins []string, next http.Handler) http.Handler {
	if len(allowedOrigins) == 0 {
		return next
	}
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		_, isAllowed := allowed[origin]
		// Vary: Origin always, even when not allowed - a shared cache
		// must not serve this response to a different origin.
		w.Header().Add("Vary", "Origin")
		if isAllowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			if isAllowed {
				w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			// A disallowed origin's preflight gets no Access-Control-*
			// headers, so the browser blocks the real request itself -
			// same effect as a 403 without needing one.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
