// Package api implements the Connect-RPC server that exposes gateway status
// and the loopback-only V2 command-publish endpoint. It used to also expose
// V1's device listing/command-schema discovery/command publishing
// (DeviceService) - retired once the V2 device platform fully covered
// device commands and automation; see docs/decisions.md's V1-removal ADR.
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
// so PublishRaw/handleV2PublishCommand reuses the gateway's already-
// connected MQTT client (see ADR-009, unchanged from internal/commandapi).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

// CommandPublisher is the one capability this package needs from the
// gateway to publish a command - satisfied structurally by *mqtt.Gateway
// (internal/mqtt) already, with no changes needed there. Declared here, not
// imported from internal/mqtt, so this package depends on a capability, not
// a concrete type - same pattern as internal/commandapi.CommandPublisher.
type CommandPublisher interface {
	// PublishRaw publishes an already-validated command envelope straight to
	// a topic - see handleV2PublishCommand below and *mqtt.Gateway.PublishRaw's
	// doc comment for why v2 needs this.
	PublishRaw(ctx context.Context, topic string, payload []byte) error
}

// StatusProvider is the one capability this package needs to serve
// GetStatus - satisfied structurally by *mqtt.Gateway already.
type StatusProvider interface {
	Snapshot() mqtt.Snapshot
}

// QueueProvider is the one capability this package needs to serve
// GetQueueSummary - satisfied structurally by *outbox.Store directly, since
// the outbox is its own component, not backed by *mqtt.Gateway.
type QueueProvider interface {
	Snapshot(ctx context.Context) (outbox.Snapshot, error)
}

// EventProvider is the one capability this package needs to serve
// GetRecentEvents - satisfied structurally by *mqtt.Gateway already, same
// pattern as CommandPublisher/StatusProvider.
type EventProvider interface {
	RecentEvents(filter mqtt.EventFilter) (events []mqtt.ActivityEvent, hasMore bool)
}

// Config is everything the API server needs to run.
type Config struct {
	// Address is api.internal_address - a loopback address only
	// internal/apigateway's reverse proxy (same host) ever connects to.
	Address        string
	RequestTimeout time.Duration
}

// Server is the loopback-only Connect-RPC server (gRPC, gRPC-Web and
// HTTP/JSON on one port).
type Server struct {
	cfg       Config
	publisher CommandPublisher
	status    StatusProvider
	queue     QueueProvider
	events    EventProvider
	logger    *slog.Logger
	http      *http.Server
}

func New(cfg Config, publisher CommandPublisher, status StatusProvider, queue QueueProvider, events EventProvider, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("api address is required")
	}
	if publisher == nil {
		return nil, errors.New("api publisher is required")
	}
	if status == nil {
		return nil, errors.New("api status provider is required")
	}
	if queue == nil {
		return nil, errors.New("api queue provider is required")
	}
	if events == nil {
		return nil, errors.New("api event provider is required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg:       cfg,
		publisher: publisher,
		status:    status,
		queue:     queue,
		events:    events,
		logger:    logger,
	}

	mux := http.NewServeMux()
	gatewayPath, gatewayHandler := apiv1connect.NewGatewayServiceHandler(s)
	mux.Handle(gatewayPath, gatewayHandler)
	mux.HandleFunc("/internal/v2/publish-command", s.handleV2PublishCommand)

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
