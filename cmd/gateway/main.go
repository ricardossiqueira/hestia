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
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/admin"
	"github.com/ricardossiqueira/iot-gateway/internal/api"
	"github.com/ricardossiqueira/iot-gateway/internal/apigateway"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/diagnostics"
	"github.com/ricardossiqueira/iot-gateway/internal/dynsec"
	gatewaymqtt "github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

const (
	testCommandTimeout         = 10 * time.Second
	diagnosticsShutdownTimeout = 5 * time.Second
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
	deviceRegistry, err := registry.Open(context.Background(), cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(stderr, "registry setup failed: %v\n", err)
		return 1
	}
	defer func() { _ = deviceRegistry.Close() }()
	if _, err := deviceRegistry.Seed(context.Background(), cfg.Devices, cfg.Routes); err != nil {
		fmt.Fprintf(stderr, "registry import failed: %v\n", err)
		return 1
	}
	snapshot, err := deviceRegistry.Snapshot(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "registry read failed: %v\n", err)
		return 1
	}
	cfg.Devices, cfg.Routes = snapshot.Devices, snapshot.Routes
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
	runtimeSnapshot := snapshot
	runtimeConfig := cfg
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	gateway, err := gatewaymqtt.New(runtimeConfig, client, gatewaymqtt.NewSlogLogger(logger), store)
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
	go watchRegistry(ctx, deviceRegistry, cfg, runtimeSnapshot.Revision, gateway, logger)
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
			Address:          cfg.API.InternalAddress,
			RequestTimeout:   apiRequestTimeout,
			Registry:         runtimeConfig,
			DeviceProvider:   gateway,
			ManifestResolver: deviceRegistry,
		}, gateway, gateway, gateway, store, gateway, logger)
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

// watchRegistry turns a durable registry revision into a live MQTT policy.
// Polling keeps the two process topology simple: the admin process and the
// sandboxed gateway only share SQLite, not a privileged in-process channel.
func watchRegistry(ctx context.Context, store *registry.Store, base config.Config, revision uint64, gateway *gatewaymqtt.Gateway, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot, err := store.Snapshot(ctx)
			if err != nil {
				logger.Error("read device registry", "error", err)
				continue
			}
			if snapshot.Revision == revision {
				continue
			}
			updated := base
			updated.Devices, updated.Routes = snapshot.Devices, snapshot.Routes
			if err := gateway.Apply(ctx, updated); err != nil {
				logger.Error("apply device registry revision", "revision", snapshot.Revision, "error", err)
				continue
			}
			revision = snapshot.Revision
			logger.Info("applied device registry revision", "revision", revision)
		}
	}
}

// runAdmin is the root-privileged process that serves DeviceAdminService
// and reverse-proxies DeviceService/GatewayService to the sandboxed `run`
// process - see internal/admin's package doc and docs/decisions.md
// ADR-008/ADR-013 for why this cannot live inside the sandboxed process.
// It has no HTTP surface of its own any more: a JSON/HTML UI on port 8081
// used to live here, retired once gateway-web reached parity with it
// (ADR-015) - internal/apigateway is now the only listener this runs.
func runAdmin(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	provisionScript := flags.String("provision-script", "deploy/mosquitto-provision-device.sh", "path to the Mosquitto provisioning script")
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
	// Without an HTML UI, this process has nothing to do at all unless
	// "api:" is configured - unlike before, when the UI itself was always
	// a reason to keep it running.
	if cfg.API == nil {
		fmt.Fprintln(stderr, "api: is not configured in gateway.yaml - this process no longer serves an HTML UI, so there is nothing for it to do (see docs/api-v1.md)")
		return 1
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	deviceRegistry, err := registry.Open(context.Background(), cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(stderr, "admin registry setup failed: %v\n", err)
		return 1
	}
	defer func() { _ = deviceRegistry.Close() }()
	if _, err := deviceRegistry.Seed(context.Background(), cfg.Devices, cfg.Routes); err != nil {
		fmt.Fprintf(stderr, "admin registry import failed: %v\n", err)
		return 1
	}
	credentials, dynsecURL, err := dynsecCredentialsFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "admin DynSec setup failed: %v\n", err)
		return 1
	}
	if dynsecURL != "" && !sameMQTTEndpoint(cfg.MQTT.URL, dynsecURL) {
		fmt.Fprintln(stderr, "admin DynSec setup failed: IOT_GATEWAY_DYNSEC_URL must target the same broker endpoint as mqtt.url")
		return 1
	}
	adminEngine, err := admin.New(admin.Config{
		ConfigPath:       *configPath,
		ProvisionScript:  *provisionScript,
		Credentials:      credentials,
		Registry:         deviceRegistry,
		DeviceBrokerHost: strings.TrimSpace(os.Getenv("IOT_GATEWAY_DEVICE_MQTT_HOST")),
		DeviceBrokerPort: mqttPort(cfg.MQTT.URL),
		RequestTimeout:   adminRequestTimeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "admin setup failed: %v\n", err)
		return 1
	}

	// Credentials are env-only, never a flag: a flag value would leak into
	// `ps` output and shell history the way the MQTT credentials
	// (username_env/password_env) already avoid for the exact same reason.
	apiUsername := os.Getenv("IOT_GATEWAY_API_USERNAME")
	apiPassword := os.Getenv("IOT_GATEWAY_API_PASSWORD")
	if apiUsername == "" || apiPassword == "" {
		fmt.Fprintln(stderr, "IOT_GATEWAY_API_USERNAME and IOT_GATEWAY_API_PASSWORD must both be set")
		return 1
	}
	apigatewayServer, err := apigateway.New(apigateway.Config{
		Address:        cfg.API.Address,
		InternalAPIURL: "http://" + cfg.API.InternalAddress,
		Credentials:    apigateway.Credentials{Username: apiUsername, Password: apiPassword},
		AllowedOrigins: cfg.API.AllowedOrigins,
		Admin:          adminEngine,
	}, logger)
	if err != nil {
		fmt.Fprintf(stderr, "api gateway setup failed: %v\n", err)
		return 1
	}
	if err := apigatewayServer.Start(); err != nil {
		fmt.Fprintf(stderr, "api gateway failed to start: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("admin process running", "api_address", cfg.API.Address)
	<-ctx.Done()
	logger.Info("admin process stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), apigatewayShutdownTimeout)
	defer cancel()
	if err := apigatewayServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("api gateway shutdown failed", "error", err)
	}
	return 0
}

// mqttPort is intentionally derived from the gateway's own broker URL, so a
// CYD receives the same listener port the long-lived gateway is using. The
// LAN-reachable host is supplied separately via IOT_GATEWAY_DEVICE_MQTT_HOST:
// mqtt.url commonly uses 127.0.0.1, which is correct for the Orange Pi but
// would be wrong when written into an ESP.
func mqttPort(rawURL string) uint16 {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Port() == "" {
		return 1883
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return 0
	}
	return uint16(port)
}

// dynsecCredentialsFromEnvironment is intentionally opt-in during migration.
// An unset URL preserves the legacy script path until the Orange Pi has a
// validated DynSec broker. The password is read from a root-owned credential
// file, never from YAML, SQLite, a command-line flag, or process arguments.
func dynsecCredentialsFromEnvironment() (admin.CredentialStore, string, error) {
	brokerURL := os.Getenv("IOT_GATEWAY_DYNSEC_URL")
	if brokerURL == "" {
		return nil, "", nil
	}
	username := os.Getenv("IOT_GATEWAY_DYNSEC_ADMIN_USERNAME")
	passwordPath := os.Getenv("IOT_GATEWAY_DYNSEC_ADMIN_PASSWORD_FILE")
	if username == "" || passwordPath == "" {
		return nil, "", errors.New("IOT_GATEWAY_DYNSEC_ADMIN_USERNAME and IOT_GATEWAY_DYNSEC_ADMIN_PASSWORD_FILE are required when IOT_GATEWAY_DYNSEC_URL is set")
	}
	passwordBytes, err := os.ReadFile(passwordPath)
	if err != nil {
		return nil, "", fmt.Errorf("read DynSec admin password file: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))
	if password == "" {
		return nil, "", errors.New("DynSec admin password file is empty")
	}
	controller, err := dynsec.NewPahoController(brokerURL, username, password)
	if err != nil {
		return nil, "", err
	}
	manager, err := dynsec.NewManager(controller)
	return manager, brokerURL, err
}

func sameMQTTEndpoint(left, right string) bool {
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	if leftErr != nil || rightErr != nil || leftURL.Hostname() == "" || rightURL.Hostname() == "" {
		return false
	}
	leftPort, rightPort := leftURL.Port(), rightURL.Port()
	if leftPort == "" {
		leftPort = "1883"
	}
	if rightPort == "" {
		rightPort = "1883"
	}
	return strings.EqualFold(leftURL.Hostname(), rightURL.Hostname()) && leftPort == rightPort
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
	deviceRegistry, err := registry.Open(context.Background(), cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(stderr, "registry setup failed: %v\n", err)
		return 1
	}
	defer func() { _ = deviceRegistry.Close() }()
	if _, err := deviceRegistry.Seed(context.Background(), cfg.Devices, cfg.Routes); err != nil {
		fmt.Fprintf(stderr, "registry import failed: %v\n", err)
		return 1
	}
	snapshot, err := deviceRegistry.Snapshot(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "registry read failed: %v\n", err)
		return 1
	}
	cfg.Devices, cfg.Routes = snapshot.Devices, snapshot.Routes
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
