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

# Build the gateway binary for the current platform.
build:
    go build -o bin/iot-gateway ./cmd/gateway

# Validate the declarative gateway configuration.
validate config=config:
    go run ./cmd/gateway validate --config {{config}}

# Run the gateway in the foreground. MQTT credential variables must be set.
run config=config:
    go run ./cmd/gateway run --config {{config}}

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
