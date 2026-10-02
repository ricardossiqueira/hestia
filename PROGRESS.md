# Device Platform v2 — gateway progress

## Current milestone

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
