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

# Build and install the gateway binary and its systemd unit on an Orange Pi.
# Safe to run on every update: never touches gateway.yaml, which is managed
# live (by hand, or via gateway-web's DeviceAdminService) after the first
# install - see install-config below and deploy/README.md.
install-binary:
    go build -o bin/iot-gateway ./cmd/gateway
    id iot-gateway >/dev/null 2>&1 || sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin iot-gateway
    sudo install -d -m 0750 -o root -g iot-gateway /etc/iot-gateway
    sudo install -d -m 0750 -o iot-gateway -g iot-gateway /var/lib/iot-gateway
    sudo install -m 0755 bin/iot-gateway /usr/local/bin/iot-gateway
    sudo install -m 0644 deploy/iot-gateway.service /etc/systemd/system/iot-gateway.service
    sudo systemctl daemon-reload

# Install the INITIAL gateway.yaml on a fresh Orange Pi. Never run this
# again after the first install: gateway.yaml is managed live from then on
# (by hand, or via gateway-web's device registration) and this recipe would
# silently overwrite it with the local, gitignored config/gateway.yaml file
# - exactly the incident documented in HANDOFF.md.
install-config config=config:
    sudo install -m 0640 -o root -g iot-gateway {{config}} /etc/iot-gateway/gateway.yaml

# Deprecated alias kept only so muscle memory doesn't silently do the wrong
# thing: fails loudly instead of overwriting a live gateway.yaml. Use
# install-binary for a routine update, or install-binary + install-config
# together only for the very first install on a fresh Orange Pi.
install-service:
    @echo "install-service was split: use 'just install-binary' for a routine update."
    @echo "Only on a FRESH Orange Pi, also run 'just install-config' once - see deploy/README.md."
    @exit 1

# Enable the service now and on subsequent boots.
enable-service:
    sudo systemctl enable --now iot-gateway.service

# Display service status.
service-status:
    systemctl status iot-gateway.service

# Follow service logs.
service-logs:
    journalctl -u iot-gateway.service -f

# Grant (or update) a device's MQTT credential + ACL on the Orange Pi's
# Mosquitto. topics: one or more of telemetry state event command
# command-result. Prints the secrets.h snippet on success.
# Usage: sudo just provision-device esp32c3-led state command
provision-device device +topics:
    sudo deploy/mosquitto-provision-device.sh {{device}} {{topics}}

# Revoke a device's MQTT credential and ACL. Also disable it in gateway.yaml.
remove-device device:
    sudo deploy/mosquitto-provision-device.sh --remove {{device}}

# Install or update the admin systemd unit (device administration API,
# served on api.address alongside DeviceService/GatewayService - see
# docs/api-v1.md). Create /etc/iot-gateway/admin-environment with
# IOT_GATEWAY_API_USERNAME and IOT_GATEWAY_API_PASSWORD before enabling it
# - see deploy/README.md.
install-admin-service:
    go build -o bin/iot-gateway ./cmd/gateway
    sudo install -m 0755 bin/iot-gateway /usr/local/bin/iot-gateway
    sudo install -m 0755 deploy/mosquitto-provision-device.sh /usr/local/bin/mosquitto-provision-device
    sudo install -m 0644 deploy/iot-gateway-admin.service /etc/systemd/system/iot-gateway-admin.service
    sudo systemctl daemon-reload

# Enable the admin service now and on subsequent boots.
enable-admin-service:
    sudo systemctl enable --now iot-gateway-admin.service

# Display admin service status.
admin-status:
    systemctl status iot-gateway-admin.service

# Follow admin service logs.
admin-logs:
    journalctl -u iot-gateway-admin.service -f

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
