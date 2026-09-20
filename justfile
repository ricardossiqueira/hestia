# IoT Gateway - common development and Orange Pi deployment tasks.
# Install just: https://github.com/casey/just

# On Windows, use PowerShell for the development-only recipes.
set windows-shell := ["powershell.exe", "-NoLogo", "-NoProfile", "-Command"]

config := "config/gateway.yaml"

# List available recipes.
default:
    @just --list

# Format Go source files.
fmt:
    go fmt ./...

# Run the unit test suite.
test:
    go test ./...

# Run static analysis.
vet:
    go vet ./...

# Run all non-mutating local checks.
check: test vet

# Validate the versioned Protobuf API. Requires Buf 1.73.0; see docs/uplink-v1.md.
proto-lint:
    buf lint

# Regenerate checked-in Go Protobuf and gRPC stubs from api/proto.
proto-generate:
    buf generate

# Regenerate stubs and fail if the working tree is no longer reproducible.
proto-check: proto-lint proto-generate
    git diff --exit-code -- api/gen/go

# Build the gateway binary for the current platform.
build:
    go build -o bin/iot-gateway ./cmd/gateway

# Validate the declarative gateway configuration.
validate config=config:
    go run ./cmd/gateway validate --config {{config}}

# Run the gateway in the foreground. MQTT credential variables must be set.
run config=config:
    go run ./cmd/gateway run --config {{config}}

# Verify the local diagnostics endpoint. This does not read MQTT credentials
# or open the SQLite outbox.
healthcheck config=config:
    go run ./cmd/gateway healthcheck --config {{config}}

# Publish a generic test command to a configured device.
publish-test-command device config=config:
    go run ./cmd/gateway publish-test-command --config {{config}} --device {{device}}

# Install or update the systemd unit on an Orange Pi. Create the environment
# file with MQTT_GATEWAY_USERNAME and MQTT_GATEWAY_PASSWORD before enabling it.
install-service config=config:
    go build -o bin/iot-gateway ./cmd/gateway
    id iot-gateway >/dev/null 2>&1 || sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin iot-gateway
    sudo install -d -m 0750 -o root -g iot-gateway /etc/iot-gateway
    sudo install -d -m 0750 -o iot-gateway -g iot-gateway /var/lib/iot-gateway
    sudo install -m 0755 bin/iot-gateway /usr/local/bin/iot-gateway
    sudo install -m 0640 -o root -g iot-gateway {{config}} /etc/iot-gateway/gateway.yaml
    sudo install -m 0644 deploy/iot-gateway.service /etc/systemd/system/iot-gateway.service
    sudo systemctl daemon-reload

# Enable the service now and on subsequent boots.
enable-service:
    sudo systemctl enable --now iot-gateway.service

# Display service status.
service-status:
    systemctl status iot-gateway.service

# Follow service logs.
service-logs:
    journalctl -u iot-gateway.service -f

# Install the root-owned pull-based deployment agent for an Orange Pi.
install-update-agent:
    sudo install -m 0755 deploy/iot-gateway-update.sh /usr/local/sbin/iot-gateway-update
    sudo install -m 0644 deploy/iot-gateway-update.service /etc/systemd/system/iot-gateway-update.service
    sudo install -m 0644 deploy/iot-gateway-update.timer /etc/systemd/system/iot-gateway-update.timer
    sudo systemctl daemon-reload

# Check for a new main revision every five minutes and apply it safely.
enable-update-agent:
    sudo systemctl enable --now iot-gateway-update.timer

# Display the schedule and last result of automatic deployment.
update-agent-status:
    systemctl status iot-gateway-update.timer
    systemctl status iot-gateway-update.service

# Follow automatic deployment logs.
update-agent-logs:
    journalctl -u iot-gateway-update.service -f
