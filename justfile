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

# Grant (or update) a device's MQTT credential + ACL on the Orange Pi's
# Mosquitto. topics: one or more of telemetry state event command
# command-result. Prints the secrets.h snippet on success.
# Usage: sudo just provision-device esp32c3-led state command
provision-device device +topics:
    sudo deploy/mosquitto-provision-device.sh {{device}} {{topics}}

# Revoke a device's MQTT credential and ACL. Also disable it in gateway.yaml.
remove-device device:
    sudo deploy/mosquitto-provision-device.sh --remove {{device}}
