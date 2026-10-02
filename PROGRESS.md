# Device Platform v2 — gateway progress

## Current milestone

**Marco 3 - secure registration transaction (in progress).** Updated 2026-10-01.

- Closed the deterministic HKDF salt/info contract with `iot-device-core`.
- Added a device-facing X25519/HKDF/AES-GCM client that validates Ed25519
  device identity signatures, encrypts provisioning settings with bound AAD,
  verifies the signed confirmation and wipes the session key after use.
- Added an in-process HTTP device test for the pair/provision protocol.
- Added the privileged registration coordinator. It persists a pending
  manifest binding, creates a disabled DynSec identity, sends the password
  only in the encrypted session, then enables DynSec and activates the
  registry binding after the device's signed persistence confirmation.
- Delivery failures revoke the new credential and remove the pending binding;
  failures after device persistence remain visibly pending for recovery.
- Next integration step is mounting this coordinator in the authenticated v2
  registration endpoint and gateway composition root.

**Marco 2 - v2 MQTT automation runtime (complete).** Updated 2026-10-01.

Completed in this milestone:

- Added a v2-only MQTT runtime that derives subscriptions from active device
  bindings and validates every MQTT output against its bound manifest before
  a rule can observe it.
- Added event-type matching and `state` retained-replay suppression, plus
  JSONLogic conditions, five-per-ten-second rate limiting, and output/command
  revalidation immediately before publish.
- Added durable `(rule_id, message_id)` execution reservations/audit, so a
  duplicate MQTT delivery cannot issue a second command across reconnects.
- Wired active v2 bindings into normal gateway startup. `EnableV2Runtime` is
  also safe to refresh after a later provisioning activation.

**Marco 1 — manifest and registry vertical slice (in progress).** Updated
2026-10-01.

Completed in this gateway repository:

- Added the isolated `internal/devicev2` manifest v2 parser/validator.
  It has explicit device-facing `mqtt.publish` / `mqtt.subscribe`, canonical
  JSON + SHA-256, schema validation, command lookup and output ACL derivation.
- Added durable v2 catalog and device binding tables in migration `010`.
  Catalog entries are immutable by hash/revision; physical instances bind a
  unique `device_uid`, observed firmware version and identity public key.
- Added pending → active/disabled/removed state persistence APIs for the
  registration/provisioning coordinator.
- Added a `dynsec.Manager.ProvisionV2` adapter that receives only a v2
  manifest and derives narrow broker permissions from it.
- Added an expiring (90 s by default) discovery inbox and a concrete HTTP
  inspector for `GET /v1/device-info`. Inspection binds an mDNS sighting to
  its UID/model and recomputes the manifest hash before an entry can advance
  to pairing/registration state.
- Aligned canonicalization with `iot-device-core`: publish channels, event
  definitions, command definitions and schema fields have order-independent
  hashes; unknown schema properties are rejected.
- Added three shared v2 manifest fixtures under `contracts/device-v2`.
- Added authenticated-admin mount support and the Web-compatible JSON RPC
  adapter: `ListDiscovery`, `RegisterDiscoveredDevice`, `ListDevices`,
  `GetDevice`, `ListAutomationRules` and `CreateAutomationRule`.
  Registration deliberately stops after a pending binding; it never invents
  a pairing proof or sends an MQTT password over plaintext HTTP.
- Added durable v2 trigger/action rules, validated exclusively against the
  bound source output and target command interfaces. The runtime executor is
  the remaining step before these rules can fire MQTT commands.

Still pending for this front:

- mDNS network browser adapter plus pairing/provisioning client. The inbox and
  inspection contract are implemented; the actual mDNS listener and encrypted
  transport await the core protocol decision.
- Mount the Web's JSON RPC adapter in the authenticated admin API. The
  Gateway-domain methods/data now exist, but the shared public Connect proto
  is still v1 and must not be changed concurrently with the other API work.
- Runtime MQTT payload validation and v2 automation trigger channels.
- replacing the legacy v1 registry/API paths after the v2 end-to-end path is
  covered.

## Interfaces available now

- `devicev2.Parse(raw) -> Manifest, canonicalJSON, sha256` validates the
  compiled firmware manifest.
- `devicev2.DeriveACL(deviceID, manifest)` returns only device write grants
  for outputs and read grants for inputs.
- `registry.AcceptV2Manifest`, `RegisterV2Device`, `SetV2DeviceState`,
  `ResolveV2Manifest` provide the persistent registration core.
- `dynsec.Manager.ProvisionV2(ctx, deviceID, manifest)` creates the matching
  narrow DynSec role/client.

## Verification

- `go test ./...` passed on 2026-10-01 (including devicev2, registry and
  DynSec tests), with the Windows Go build cache accessed through the approved
  test runner.
