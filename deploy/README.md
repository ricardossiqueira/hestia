# Implantacao com systemd

`iot-gateway.service` executa o gateway como o usuario de sistema sem login
`iot-gateway`. A configuracao YAML fica em `/etc/iot-gateway/gateway.yaml` e
as credenciais MQTT ficam em `/etc/iot-gateway/environment`, separadas do
repositorio.

No Orange Pi, depois de atualizar o projeto, instale o binario e a unidade:

```bash
cd ~/iot-gateway
go build -o bin/iot-gateway ./cmd/gateway

id iot-gateway >/dev/null 2>&1 || \
  sudo useradd --system --user-group --no-create-home \
  --shell /usr/sbin/nologin iot-gateway

sudo install -d -m 0750 -o root -g iot-gateway /etc/iot-gateway
sudo install -d -m 0750 -o iot-gateway -g iot-gateway /var/lib/iot-gateway
sudo install -m 0755 bin/iot-gateway /usr/local/bin/iot-gateway
sudo install -m 0640 -o root -g iot-gateway \
  config/gateway.yaml /etc/iot-gateway/gateway.yaml
sudo install -m 0644 deploy/iot-gateway.service \
  /etc/systemd/system/iot-gateway.service
```

Create `/etc/iot-gateway/environment` with the names configured in `mqtt`.
For the current sample configuration:

```ini
MQTT_GATEWAY_USERNAME=gateway
MQTT_GATEWAY_PASSWORD=the-gateway-mqtt-password
```

Protect the environment file and enable the service at boot:

```bash
sudo chown root:iot-gateway /etc/iot-gateway/environment
sudo chmod 640 /etc/iot-gateway/environment
sudo systemctl daemon-reload
sudo systemctl enable --now iot-gateway.service
systemctl status iot-gateway.service
```

Follow operational logs with:

```bash
journalctl -u iot-gateway.service -f
```

## Atualizacao para a outbox SQLite

Before installing a version with the durable outbox, add this required field
to `/etc/iot-gateway/gateway.yaml` under `storage`:

```yaml
max_outbox_bytes: 33554432
```

`33554432` is 32 MiB of MQTT payloads. It is a logical payload limit, not the
SQLite file size. The gateway refuses to start without an explicit positive
limit, preventing one large message or a long VPS outage from exhausting the
microSD card. Validate, install the updated binary, and restart the service:

```bash
sudo -u iot-gateway /usr/local/bin/iot-gateway validate --config /etc/iot-gateway/gateway.yaml
just install-binary
sudo systemctl restart iot-gateway.service
```

## CI and automatic updates

`.github/workflows/ci.yml` runs formatting, tests, vet, and a Linux ARM64
build for every push or pull request targeting `main`. The Orange Pi does not
accept inbound connections from GitHub. Instead, `iot-gateway-update.timer`
checks `main` every five minutes, runs tests, vet, build, and configuration
validation locally, then restarts the gateway only after those checks pass.

The updater never copies a configuration file from Git and never reads or
writes `/etc/iot-gateway/environment`. It uses only a fast-forward Git update.
If the new service fails to start or exits immediately, it restores the
previous binary and unit. After a successful restart it also executes
`iot-gateway healthcheck --config /etc/iot-gateway/gateway.yaml` as the
restricted `iot-gateway` user exactly ten times, one second apart. The update
is committed only when one attempt receives HTTP 200 with `{"status":"ok"}`
from the configured loopback `/healthz` endpoint. A failed healthcheck rolls
back the previous binary and unit. On the first installation there is no prior
binary or unit to restore; in that case the updater leaves the failed service
stopped and logs this explicitly.

If `iot-gateway-admin.service` is installed (see "Admin UI" below), the
updater also restarts it once `iot-gateway.service` passes its healthcheck -
both services `ExecStart` the same `/usr/local/bin/iot-gateway` binary, and
since `docs/decisions.md` ADR-013, admin is the only public listener for the
local API (`internal/apigateway`). It has no reason to restart on its own,
so leaving this step out would silently strand it on the old binary on every
future update that touches the API - which is exactly what happened once in
production before this was added. If admin fails to come back up, the
updater rolls back **both** services to the previous binary together, never
just the core gateway - an installation with no admin service configured
skips this step entirely.

You can query the same payload-free endpoint manually, without MQTT
credentials or SQLite access:

```bash
sudo -u iot-gateway /usr/local/bin/iot-gateway healthcheck \
  --config /etc/iot-gateway/gateway.yaml
```

### One-time Git read access

The updater runs Git as `orangepi`, so configure a read-only deploy key for
this repository while logged in as that user:

```bash
mkdir -p ~/.ssh
chmod 700 ~/.ssh
ssh-keygen -t ed25519 -f ~/.ssh/iot-gateway-deploy -C orangepi-iot-gateway-deploy
cat ~/.ssh/iot-gateway-deploy.pub
```

Add the public key in GitHub under the `iot-gateway` repository's **Settings**
then **Deploy keys**. Leave write access disabled. Configure the repository to
use that key:

```bash
nano ~/.ssh/config
```

Add this entry:

```text
Host github.com-iot-gateway
    HostName github.com
    User git
    IdentityFile ~/.ssh/iot-gateway-deploy
    IdentitiesOnly yes
```

Then secure it and change the existing clone remote:

```bash
chmod 600 ~/.ssh/config ~/.ssh/iot-gateway-deploy
ssh -T git@github.com-iot-gateway
cd ~/iot-gateway
git remote set-url origin git@github.com-iot-gateway:ricardossiqueira/iot-gateway.git
git fetch origin main
```

### Enable the update agent

After updating the repository to a version containing these files, run:

```bash
cd ~/iot-gateway
just install-update-agent
just enable-update-agent
sudo systemctl start iot-gateway-update.service
just update-agent-status
```

The first start records the deployed revision. Later executions deploy only a
new `main` revision, including future changes to the updater itself. Use `just
update-agent-logs` to inspect an update; a failed build, invalid YAML, dirty
worktree, or failed restart leaves the currently installed gateway running.

The repository is intentionally user-writable because Git and compilation run
as `orangepi`, but the resulting approved `main` revision is installed by root.
Treat write access to this repository, its deploy key, and direct pushes to
`main` as control of the Orange Pi. Keep `orangepi` limited to trusted users
and protect `main` with the CI workflow in GitHub before enabling this agent.

## Device administration

Registering, enabling/disabling and removing a device (without SSH-ing in)
goes through `DeviceAdminService` on the local API below - `gateway-web` is
the day-to-day client. It creates the Mosquitto credential/ACL (by calling
`deploy/mosquitto-provision-device.sh` under the hood), writes the entry to
`gateway.yaml`, and restarts `iot-gateway.service` to apply it - the same
thing a small LAN-facing HTML page (`iot-gateway-admin.service`, port 8081)
used to do before `gateway-web` reached parity with it (`docs/decisions.md`
ADR-015). `iot-gateway-admin.service` still exists and still must run as
its **own** root process - see ADR-008 for why (writing
`/etc/mosquitto/*`/`gateway.yaml` and calling `systemctl` can never happen
in the sandboxed gateway process) - it just has no HTML of its own left to
open in a browser. `docs/mosquitto-device-provisioning.md` documents what
the underlying script automates, for when you need to debug it directly.

## Local API (Connect-RPC)

Listing devices, discovering what commands they accept, publishing a
command, reading gateway status, and device administration
(`DeviceAdminService`, above) all go through one Connect-RPC API (gRPC,
gRPC-Web and HTTP/JSON on one port). See `docs/api-v1.md` for the full
contract, the RPCs, and `curl` examples.

**Two processes compose it (`docs/decisions.md` ADR-013)** - this matters
for where credentials and env vars go:

- `iot-gateway-admin.service` (root) binds `api.address` and is the
  **only public listener**. It authenticates (HTTP Basic) and applies
  CORS, and answers `DeviceAdminService` directly.
- `iot-gateway.service` (sandboxed) binds `api.internal_address`
  (loopback-only, defaults to `127.0.0.1:8083`, normally not set
  explicitly) and answers `DeviceService`/`GatewayService` - the admin
  process reverse-proxies to it. It needs no credentials of its own: the
  loopback binding is its whole trust boundary.

Both processes read the **same** `api:` section in `gateway.yaml` - no
duplication needed there. It is optional and off by default - add the
section (see `configs/gateway.example.yaml`) to turn the whole thing on.
Since `iot-gateway-admin.service` has nothing else to do without it, `admin`
now refuses to start at all if `api:` is absent.

**LAN-trusted only. Never port-forward or expose this to the internet.**
Plain HTTP (no TLS), one shared HTTP Basic Auth credential, from the
**admin** environment file:

```bash
openssl rand -base64 24
sudo install -d -m 0750 -o root -g root /etc/iot-gateway
sudoedit /etc/iot-gateway/admin-environment
```

```ini
IOT_GATEWAY_API_USERNAME=api
IOT_GATEWAY_API_PASSWORD=the-generated-password
```

```bash
sudo chown root:root /etc/iot-gateway/admin-environment
sudo chmod 600 /etc/iot-gateway/admin-environment
```

Install and enable:

```bash
just install-admin-service
just enable-admin-service
just admin-status
```

If this file still has `IOT_GATEWAY_ADMIN_USERNAME`/`PASSWORD` from before
the HTML UI was retired, they are simply unused now - safe to remove or to
leave.

`iot-gateway.service`'s own `/etc/iot-gateway/environment` does **not**
need `IOT_GATEWAY_API_USERNAME`/`PASSWORD` - only
`MQTT_GATEWAY_USERNAME`/`PASSWORD`. If a previous install left the API
variables there, they are simply unused now; safe to remove or to leave.

### CORS (for a browser client, e.g. `gateway-web`)

Off by default - a browser cannot call this API cross-origin at all unless
`cors_allowed_origins` is set. To let a browser-based client (see
`gateway-web/docs/spec.md`) call it directly, add an exact origin allowlist
next to `api.address` in `gateway.yaml` (see `configs/gateway.example.yaml`
and `docs/api-v1.md`'s CORS section - never `*`, validation rejects it):

```yaml
api:
  address: 0.0.0.0:8082
  cors_allowed_origins:
    - http://localhost:5173
```

### Migrating from before this composition existed

If `api.address` was already `0.0.0.0:8082` and answered directly by
`iot-gateway.service` (no `internal_address`, no admin-side proxy - true
for any install before this section's current form), the value itself
does not need to change: it is just read by a different process now.

1. Move `IOT_GATEWAY_API_USERNAME`/`PASSWORD` from
   `/etc/iot-gateway/environment` to `/etc/iot-gateway/admin-environment`
   (same values - no need to generate new secrets). No `.service` file
   changes needed either way.
2. Update and restart **both** services (same binary for both):

```bash
sudo -u iot-gateway /usr/local/bin/iot-gateway validate --config /etc/iot-gateway/gateway.yaml
just install-binary
sudo systemctl restart iot-gateway.service iot-gateway-admin.service
```

Send a command (`configs/gateway.example.yaml` suggests port `8082`):

```bash
curl -u api:the-generated-password \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-1","type":"set_led","parameters":{"on":true}}' \
  http://<orange-pi-lan-ip>:8082/iot.gateway.api.v1.DeviceService/PublishCommand
```
