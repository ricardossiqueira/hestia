package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

//go:embed templates/*.html
var templateFS embed.FS

var pageTemplate = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// Credentials gate every request behind HTTP Basic Auth. Populated from
// environment variables by cmd/gateway (never a flag - a flag value leaks
// into `ps` output and shell history) and never logged.
type Credentials struct {
	Username string
	Password string
}

// Config is everything the admin server needs to run.
type Config struct {
	Address         string
	ConfigPath      string
	ProvisionScript string
	Credentials     Credentials
	// RequestTimeout bounds how long a single provisioning request (which
	// shells out to a script and restarts a systemd unit) may run.
	RequestTimeout time.Duration
}

// Server is the LAN-facing device registration HTTP server. Unlike
// internal/diagnostics, it is intentionally NOT loopback-restricted - see
// docs/decisions.md ADR-008 - and every route mutates system state, so
// every route also requires Basic Auth.
type Server struct {
	cfg    Config
	logger *slog.Logger
	http   *http.Server
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("admin address is required")
	}
	if strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("admin config path is required")
	}
	if strings.TrimSpace(cfg.ProvisionScript) == "" {
		return nil, errors.New("admin provision script path is required")
	}
	if cfg.Credentials.Username == "" || cfg.Credentials.Password == "" {
		return nil, errors.New("admin username and password are required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /devices", s.handleAddDevice)
	mux.HandleFunc("POST /devices/{id}/remove", s.handleRemoveDevice)

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
		return fmt.Errorf("listen admin on %q: %w", s.cfg.Address, err)
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("admin server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

// Shutdown stops accepting admin requests and waits for active requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown admin server: %w", err)
	}
	return nil
}

func basicAuth(creds Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		validUser := subtle.ConstantTimeCompare([]byte(username), []byte(creds.Username)) == 1
		validPass := subtle.ConstantTimeCompare([]byte(password), []byte(creds.Password)) == 1
		if !ok || !validUser || !validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="iot-gateway admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pageData is the view model for templates/index.html.
type pageData struct {
	Devices []deviceView
	Flash   *flashView
	Error   string
}

type deviceView struct {
	ID            string
	Enabled       bool
	TopicsSummary string
}

type flashView struct {
	DeviceID string
	Password string
}

func (s *Server) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := pageTemplate.ExecuteTemplate(w, "index.html", data); err != nil {
		s.logger.Error("render admin page failed", "error", err)
	}
}

func (s *Server) loadPageData() (pageData, error) {
	devices, err := ListDevices(s.cfg.ConfigPath)
	if err != nil {
		return pageData{}, err
	}
	views := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		views = append(views, deviceView{
			ID:            d.ID,
			Enabled:       d.Enabled != nil && *d.Enabled,
			TopicsSummary: topicsSummary(d.Topics),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return pageData{Devices: views}, nil
}

func topicsSummary(t config.Topics) string {
	var names []string
	if t.Telemetry != "" {
		names = append(names, "telemetry")
	}
	if t.State != "" {
		names = append(names, "state")
	}
	if t.Event != "" {
		names = append(names, "event")
	}
	if t.Command != "" {
		names = append(names, "command")
	}
	if t.CommandResult != "" {
		names = append(names, "command_result")
	}
	return strings.Join(names, ", ")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadPageData()
	if err != nil {
		s.render(w, http.StatusInternalServerError, pageData{Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleAddDevice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	if err := r.ParseForm(); err != nil {
		s.renderWithError(w, "invalid form submission")
		return
	}
	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	topics := r.Form["topics"]
	if deviceID == "" {
		s.renderWithError(w, "device_id is required")
		return
	}
	if len(topics) == 0 {
		s.renderWithError(w, "select at least one topic")
		return
	}

	// Mosquitto first, gateway.yaml second - same order as the manual
	// checklist in docs/device-onboarding.md. If provisioning fails, the
	// YAML is never touched at all.
	password, err := Provision(ctx, s.cfg.ProvisionScript, deviceID, topics)
	if err != nil {
		s.renderWithError(w, fmt.Sprintf("provisioning failed: %v", err))
		return
	}
	if err := AddDevice(s.cfg.ConfigPath, deviceID, topics); err != nil {
		s.renderWithError(w, fmt.Sprintf(
			"device %q was provisioned in Mosquitto but NOT added to gateway.yaml: %v. "+
				"Fix gateway.yaml by hand, or remove the orphaned credential with the CLI script's --remove.",
			deviceID, err))
		return
	}
	if err := RestartGateway(ctx); err != nil {
		s.renderWithError(w, fmt.Sprintf(
			"device %q was registered but the gateway service failed to restart: %v. "+
				"Restart it by hand to apply the change.", deviceID, err))
		return
	}

	data, err := s.loadPageData()
	if err != nil {
		s.render(w, http.StatusInternalServerError, pageData{Error: err.Error()})
		return
	}
	data.Flash = &flashView{DeviceID: deviceID, Password: password}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	deviceID := strings.TrimSpace(r.PathValue("id"))
	if deviceID == "" {
		s.renderWithError(w, "device id is required")
		return
	}

	if err := RemoveDevice(s.cfg.ConfigPath, deviceID); err != nil {
		s.renderWithError(w, fmt.Sprintf("removing %q from gateway.yaml failed: %v", deviceID, err))
		return
	}
	if err := Deprovision(ctx, s.cfg.ProvisionScript, deviceID); err != nil {
		s.renderWithError(w, fmt.Sprintf(
			"device %q was removed from gateway.yaml but its Mosquitto credential/ACL could not be revoked: %v. "+
				"Run the CLI script's --remove by hand.", deviceID, err))
		return
	}
	if err := RestartGateway(ctx); err != nil {
		s.renderWithError(w, fmt.Sprintf("device %q was removed but the gateway service failed to restart: %v", deviceID, err))
		return
	}

	data, err := s.loadPageData()
	if err != nil {
		s.render(w, http.StatusInternalServerError, pageData{Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) renderWithError(w http.ResponseWriter, message string) {
	data, err := s.loadPageData()
	if err != nil {
		s.render(w, http.StatusInternalServerError, pageData{Error: err.Error()})
		return
	}
	data.Error = message
	s.render(w, http.StatusBadRequest, data)
}
