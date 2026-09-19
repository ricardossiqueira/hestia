// Command iot-gateway will run the local IoT gateway on the Orange Pi.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/diagnostics"
	gatewaymqtt "github.com/ricardossiqueira/iot-gateway/internal/mqtt"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

const (
	testCommandTimeout         = 10 * time.Second
	diagnosticsShutdownTimeout = 5 * time.Second
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
	case "publish-test-command":
		return runPublishTestCommand(args[1:], stderr, gatewaymqtt.NewPahoClient)
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
	logger.Info("IoT gateway running", "gateway_id", cfg.Gateway.ID)
	<-ctx.Done()
	logger.Info("IoT gateway stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), diagnosticsShutdownTimeout)
	if err := diagnosticsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("diagnostics shutdown failed", "error", err)
	}
	cancel()
	gateway.Close()
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
	fmt.Fprintln(stderr, "usage: iot-gateway <validate|run|publish-test-command> --config <path>")
}
