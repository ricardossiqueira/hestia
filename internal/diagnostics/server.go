// Package diagnostics exposes a small, local-only, read-only operational API.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

// GatewaySnapshotter is the payload-free runtime state required by diagnostics.
type GatewaySnapshotter interface {
	Snapshot() mqtt.Snapshot
}

// OutboxSnapshotter is the read-only outbox query required by diagnostics.
type OutboxSnapshotter interface {
	Snapshot(context.Context) (outbox.Snapshot, error)
}

// Server is the local diagnostics HTTP server.
type Server struct {
	address string
	timeout time.Duration
	gateway GatewaySnapshotter
	outbox  OutboxSnapshotter
	http    *http.Server
}

// New builds a diagnostics server. It deliberately repeats the loopback check
// so callers that construct configuration in memory cannot expose it remotely.
func New(cfg config.Diagnostics, gateway GatewaySnapshotter, queue OutboxSnapshotter) (*Server, error) {
	if gateway == nil {
		return nil, errors.New("diagnostics gateway is required")
	}
	if queue == nil {
		return nil, errors.New("diagnostics outbox is required")
	}
	if err := validateLoopbackAddress(cfg.Address); err != nil {
		return nil, err
	}
	if cfg.RequestTimeout.TimeDuration() <= 0 || cfg.RequestTimeout.TimeDuration() > 10*time.Second {
		return nil, errors.New("diagnostics request timeout must be greater than zero and at most 10s")
	}
	server := &Server{address: cfg.Address, timeout: cfg.RequestTimeout.TimeDuration(), gateway: gateway, outbox: queue}
	server.http = &http.Server{
		Addr:              server.address,
		Handler:           http.HandlerFunc(server.handle),
		ReadHeaderTimeout: server.timeout,
		ReadTimeout:       server.timeout,
		WriteTimeout:      server.timeout,
		IdleTimeout:       server.timeout,
	}
	return server, nil
}

// Start begins serving in the background after binding the configured local
// address. Binding failures are reported synchronously to the caller.
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen diagnostics on %q: %w", s.address, err)
	}
	go func() {
		_ = s.http.Serve(listener)
	}()
	return nil
}

// Shutdown stops accepting diagnostics requests and waits for active requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown diagnostics server: %w", err)
	}
	return nil
}

func (s *Server) handle(writer http.ResponseWriter, request *http.Request) {
	setHeaders(writer)
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	switch request.URL.Path {
	case "/healthz":
		s.handleHealth(writer)
	case "/status":
		s.handleStatus(writer, request)
	default:
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (s *Server) handleHealth(writer http.ResponseWriter) {
	snapshot := s.gateway.Snapshot()
	if !snapshot.Started || !snapshot.MQTTConnected {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), s.timeout)
	defer cancel()
	queue, err := s.outbox.Snapshot(ctx)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "outbox unavailable"})
		return
	}
	gateway := s.gateway.Snapshot()
	writeJSON(writer, http.StatusOK, statusResponse{
		StartedAt: gateway.StartedAt,
		MQTT: mqttResponse{
			Connected:     gateway.MQTTConnected,
			Subscriptions: gateway.Subscriptions,
		},
		Messages: messageResponse{
			Accepted:             gateway.AcceptedMessages,
			Rejected:             gateway.RejectedMessages,
			LocalRoutesPublished: gateway.LocalRoutesPublished,
			LocalRoutesFailed:    gateway.LocalRoutesFailed,
		},
		Outbox: outboxResponse{
			Messages:         queue.Messages,
			PayloadBytes:     queue.PayloadBytes,
			OldestEnqueuedAt: queue.OldestEnqueuedAt,
			Stored:           gateway.OutboxStored,
			Discarded:        gateway.OutboxDiscarded,
			Failed:           gateway.OutboxFailed,
		},
	})
}

type statusResponse struct {
	StartedAt *time.Time      `json:"started_at"`
	MQTT      mqttResponse    `json:"mqtt"`
	Messages  messageResponse `json:"messages"`
	Outbox    outboxResponse  `json:"outbox"`
}

type mqttResponse struct {
	Connected     bool `json:"connected"`
	Subscriptions int  `json:"subscriptions"`
}

type messageResponse struct {
	Accepted             uint64 `json:"accepted"`
	Rejected             uint64 `json:"rejected"`
	LocalRoutesPublished uint64 `json:"local_routes_published"`
	LocalRoutesFailed    uint64 `json:"local_routes_failed"`
}

type outboxResponse struct {
	Messages         int        `json:"messages"`
	PayloadBytes     int64      `json:"payload_bytes"`
	OldestEnqueuedAt *time.Time `json:"oldest_enqueued_at"`
	Stored           uint64     `json:"stored"`
	Discarded        uint64     `json:"discarded"`
	Failed           uint64     `json:"failed"`
}

func setHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func validateLoopbackAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("diagnostics address must be an IP literal and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("diagnostics address must use a loopback IP literal")
	}
	port, err := strconv.Atoi(portText)
	if strings.Trim(portText, "0123456789") != "" || err != nil || port < 1 || port > 65535 {
		return errors.New("diagnostics address must include a port between 1 and 65535")
	}
	return nil
}
