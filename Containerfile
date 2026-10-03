# Built on GitHub-hosted CI (linux/arm64 via QEMU), never on the Orange Pi
# itself - see .github/workflows/ci.yml's publish-image job and
# orangepi-deploy/README.md for why native builds don't happen on the
# device anymore.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/iot-gateway ./cmd/gateway

# :nonroot runs as UID/GID 65532 - the host paths bind-mounted at
# /etc/iot-gateway and /var/lib/iot-gateway must be chown'd to 65532:65532
# (see orangepi-deploy/README.md's bootstrap).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/iot-gateway /usr/local/bin/iot-gateway
ENTRYPOINT ["/usr/local/bin/iot-gateway"]
