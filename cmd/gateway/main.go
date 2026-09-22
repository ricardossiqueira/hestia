// Command iot-gateway will run the local IoT gateway on the Orange Pi.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/admin"
	"github.com/ricardossiqueira/iot-gateway/internal/api"
	"github.com/ricardossiqueira/iot-gateway/internal/apigateway"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/diagnostics"
	gatewaymqtt "github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

const (
	testCommandTimeout         = 10 * time.Second
	diagnosticsShutdownTimeout = 5 * time.Second
	adminShutdownTimeout       = 5 * time.Second
	adminRequestTimeout        = 30 * time.Second
	apiShutdownTimeout         = 5 * time.Second
	apiRequestTimeout          = 10 * time.Second
	apigatewayShutdownTimeout  = 5 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "run":
		return runGateway(args[1:], stderr)
	case "healthcheck":
		return runHealthcheck(args[1:], stdout, stderr)
	case "publish-test-command":
		return runPublishTestCommand(args[1:], stderr, gatewaymqtt.NewPahoClient)
	case "admin":
		return runAdmin(args[1:], stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

type mqttClientFactory func(config.MQTT, gatewaymqtt.Credentials) (gatewaymqtt.Client, error)

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		printUsage(stderr)
		return 2
	}

	if _, err := config.Load(*configPath); err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "configuration is valid")
	return 0
}

// runHealthcheck verifies only the local diagnostics endpoint. In particular,
// it deliberately does not resolve MQTT credentials or open the SQLite outbox.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		printUsage(stderr)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}
	if err := checkHealth(cfg); err != nil {
		fmt.Fprintf(stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "healthcheck is ok")
	return 0
}

func checkHealth(cfg config.Config) error {
	timeout := cfg.Diagnostics.RequestTimeout.TimeDuration()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Diagnostics.Address+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("create local health request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("local diagnostics endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("local diagnostics returned HTTP %d", response.StatusCode)
	}

	var payload struct {
		Status string `json:"status"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	if err := decoder.Decode(&payload); err != nil {
		return errors.New("local diagnostics returned invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("local diagnostics returned invalid JSON")
	}
	if payload.Status != "ok" {
		return errors.New("local diagnostics is not healthy")
	}
	return nil
}

func runGateway(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		printUsage(stderr)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}
	credentials, err := gatewaymqtt.ResolveCredentials(cfg.MQTT, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "MQTT credentials are invalid: %v\n", err)
		return 1
	}
	client, err := gatewaymqtt.NewPahoClient(cfg.MQTT, credentials)
	if err != nil {
		fmt.Fprintf(stderr, "MQTT client setup failed: %v\n", err)
		return 1
	}
	store, err := openOutbox(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "outbox setup failed: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	gateway, err := gatewaymqtt.New(cfg, client, gatewaymqtt.NewSlogLogger(logger), store)
	if err != nil {
		fmt.Fprintf(stderr, "MQTT gateway setup failed: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := gateway.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "MQTT gateway failed to start: %v\n", err)
		gateway.Close()
		return 1
	}
	diagnosticsServer, err := diagnostics.New(cfg.Diagnostics, gateway, store)
	if err != nil {
		fmt.Fprintf(stderr, "diagnostics setup failed: %v\n", err)
		gateway.Close()
		return 1
	}
	if err := diagnosticsServer.Start(); err != nil {
		fmt.Fprintf(stderr, "diagnostics failed to start: %v\n", err)
		gateway.Close()
		return 1
	}

	// Opt-in (nil unless "api:" is in the YAML - config.API's doc comment).
	// Binds api.internal_address, a loopback-only address (ADR-013) - never
	// LAN-reachable, so this needs no credentials of its own. Reuses this
	// SAME already-connected gateway/MQTT client rather than opening a
	// second connection, so it never fights the long-lived session for
	// mqtt.client_id the way a per-request reconnect (like
	// publish-test-command below) would if done repeatedly - see
	// docs/decisions.md ADR-009. The public, authenticated, CORS-enabled
	// edge (internal/apigateway) runs in the `admin` subcommand instead,
	// reverse-proxying here - see runAdmin below and ADR-013.
	var apiServer *api.Server
	if cfg.API != nil {
		apiServer, err = api.New(api.Config{
			Address:        cfg.API.InternalAddress,
			RequestTimeout: apiRequestTimeout,
			Registry:       cfg,
		}, gateway, gateway, logger)
		if err != nil {
			fmt.Fprintf(stderr, "api setup failed: %v\n", err)
			gateway.Close()
			return 1
		}
		if err := apiServer.Start(); err != nil {
			fmt.Fprintf(stderr, "api failed to start: %v\n", err)
			gateway.Close()
			return 1
		}
	}

	logger.Info("IoT gateway running", "gateway_id", cfg.Gateway.ID)
	<-ctx.Done()
	logger.Info("IoT gateway stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), diagnosticsShutdownTimeout)
	if err := diagnosticsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("diagnostics shutdown failed", "error", err)
	}
	cancel()
	if apiServer != nil {
		apiShutdownCtx, apiCancel := context.WithTimeout(context.Background(), apiShutdownTimeout)
		if err := apiServer.Shutdown(apiShutdownCtx); err != nil {
			logger.Error("api shutdown failed", "error", err)
		}
		apiCancel()
	}
	gateway.Close()
	return 0
}

// runAdmin serves the LAN-facing device registration UI. It is meant to run
// under its own systemd unit (deploy/iot-gateway-admin.service), as root -
// see internal/admin's package doc and docs/decisions.md ADR-008 for why
// this cannot live inside the sandboxed `run` command above.
func runAdmin(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	listen := flags.String("listen", "0.0.0.0:8081", "address the admin UI listens on")
	provisionScript := flags.String("provision-script", "deploy/mosquitto-provision-device.sh", "path to the Mosquitto provisioning script")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		printUsage(stderr)
		return 2
	}

	// Credentials are env-only, never a flag: a flag value would leak into
	// `ps` output and shell history the way the MQTT credentials
	// (username_env/password_env) already avoid for the exact same reason.
	username := os.Getenv("IOT_GATEWAY_ADMIN_USERNAME")
	password := os.Getenv("IOT_GATEWAY_ADMIN_PASSWORD")
	if username == "" || password == "" {
		fmt.Fprintln(stderr, "IOT_GATEWAY_ADMIN_USERNAME and IOT_GATEWAY_ADMIN_PASSWORD must both be set")
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	server, err := admin.New(admin.Config{
		Address:         *listen,
		ConfigPath:      *configPath,
		ProvisionScript: *provisionScript,
		Credentials:     admin.Credentials{Username: username, Password: password},
		RequestTimeout:  adminRequestTimeout,
	}, logger)
	if err != nil {
		fmt.Fprintf(stderr, "admin setup failed: %v\n", err)
		return 1
	}
	if err := server.Start(); err != nil {
		fmt.Fprintf(stderr, "admin failed to start: %v\n", err)
		return 1
	}

	// Opt-in (nil unless "api:" is in the YAML - config.API's doc
	// comment). This process, already root and already the LAN-facing
	// listener for the admin UI above, is also the public edge of the
	// Connect-RPC API (docs/decisions.md ADR-013): it authenticates,
	// applies CORS, and reverse-proxies to internal/api running inside
	// the sandboxed `iot-gateway run` process at api.internal_address.
	// DeviceAdminService (a later phase) will be answered directly here
	// instead of proxied, once it exists.
	var apigatewayServer *apigateway.Server
	if cfg.API != nil {
		apiUsername := os.Getenv("IOT_GATEWAY_API_USERNAME")
		apiPassword := os.Getenv("IOT_GATEWAY_API_PASSWORD")
		if apiUsername == "" || apiPassword == "" {
			fmt.Fprintln(stderr, "IOT_GATEWAY_API_USERNAME and IOT_GATEWAY_API_PASSWORD must both be set")
			return 1
		}
		apigatewayServer, err = apigateway.New(apigateway.Config{
			Address:        cfg.API.Address,
			InternalAPIURL: "http://" + cfg.API.InternalAddress,
			Credentials:    apigateway.Credentials{Username: apiUsername, Password: apiPassword},
			AllowedOrigins: cfg.API.AllowedOrigins,
			// The SAME *admin.Server already constructed for the HTML UI
			// above satisfies apigateway.DeviceAdmin structurally (see its
			// doc comment) - no second config, no new process.
			Admin: server,
		}, logger)
		if err != nil {
			fmt.Fprintf(stderr, "api gateway setup failed: %v\n", err)
			return 1
		}
		if err := apigatewayServer.Start(); err != nil {
			fmt.Fprintf(stderr, "api gateway failed to start: %v\n", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("admin UI running", "listen", *listen)
	<-ctx.Done()
	logger.Info("admin UI stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("admin shutdown failed", "error", err)
	}
	if apigatewayServer != nil {
		apigatewayShutdownCtx, apigatewayCancel := context.WithTimeout(context.Background(), apigatewayShutdownTimeout)
		if err := apigatewayServer.Shutdown(apigatewayShutdownCtx); err != nil {
			logger.Error("api gateway shutdown failed", "error", err)
		}
		apigatewayCancel()
	}
	return 0
}

func runPublishTestCommand(args []string, stderr io.Writer, newClient mqttClientFactory) int {
	flags := flag.NewFlagSet("publish-test-command", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	deviceID := flags.String("device", "", "ID of the enabled target device")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *deviceID == "" {
		printUsage(stderr)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}
	credentials, err := gatewaymqtt.ResolveCredentials(cfg.MQTT, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "MQTT credentials are invalid: %v\n", err)
		return 1
	}
	client, err := newClient(cfg.MQTT, credentials)
	if err != nil {
		fmt.Fprintf(stderr, "MQTT client setup failed: %v\n", err)
		return 1
	}
	gateway, err := gatewaymqtt.NewCommandPublisher(cfg, client, gatewaymqtt.NewSlogLogger(slog.New(slog.NewTextHandler(stderr, nil))))
	if err != nil {
		fmt.Fprintf(stderr, "MQTT gateway setup failed: %v\n", err)
		return 1
	}
	defer gateway.Close()
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, testCommandTimeout)
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "MQTT gateway failed to start: %v\n", err)
		return 1
	}
	payload, err := newTestCommandPayload()
	if err != nil {
		fmt.Fprintf(stderr, "failed to create test command: %v\n", err)
		return 1
	}
	if err := gateway.PublishCommand(ctx, *deviceID, payload); err != nil {
		fmt.Fprintf(stderr, "failed to publish test command: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "test command published to device %q\n", *deviceID)
	return 0
}

func newTestCommandPayload() ([]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	command := struct {
		CommandID  string         `json:"command_id"`
		Type       string         `json:"type"`
		Parameters map[string]any `json:"parameters"`
	}{
		CommandID:  fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]),
		Type:       "gateway_test",
		Parameters: map[string]any{},
	}
	return json.Marshal(command)
}

func openOutbox(cfg config.Config) (*outbox.Store, error) {
	return outbox.Open(context.Background(), cfg.Storage.SQLitePath, outbox.Options{
		MaxMessages: cfg.Storage.MaxOutboxMessages,
		MaxBytes:    cfg.Storage.MaxOutboxBytes,
		MaxAge:      cfg.Storage.MaxOutboxAge.TimeDuration(),
	})
}

func printUsage(stderr io.Writer) {
	fmt.Fprintln(stderr, "usage: iot-gateway <validate|run|healthcheck|publish-test-command|admin> --config <path>")
}
