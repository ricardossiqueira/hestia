# Device Platform v2 — gateway progress

## Current milestone

**Real-device discovery debugging — mDNS rebrowse fix (complete).** Updated 2026-10-02.

Investigated why the Orange Pi gateway could not discover `esp32c3-led`
through `ListDiscovery`, running against real hardware end-to-end (not just
fixtures). Four independent, compounding bugs were found and fixed across
three repos before discovery worked:

- Orange Pi Zero 2W's onboard Wi-Fi chip (Unisoc SC2355) doesn't receive
  IPv4 multicast reliably - fixed on the device side
  (`esp32c3-led`/`iot-device-core`), not in this repo; see
  `docs/device-connection.md` and `esp32c3-led/PROGRESS.md` for the full
  diagnostic trail (service-type mismatch, Avahi config, IGMP, AP/band all
  ruled out before landing on the chip/driver).
- Two real bugs in `iot-device-core`'s manifest canonicalization and
  hand-rolled SHA-256 (a transposed hex digit in a round constant) made
  `HTTPInspector.Inspect` reject every device with a hash mismatch - fixed
  in that repo, not this one.
- **This repo's bug**: `internal/devicev2/mdns.go`'s `MDNSBrowser.Run` called
  `zeroconf.Browse` exactly once for the process's lifetime. `grandcat/
  zeroconf` only ever delivers an entry the first time it sees a given
  service instance - it never re-emits one after that instance's TXT/address
  record changes (e.g. the device reboots with new firmware). A long-lived
  gateway process would freeze every device's Inbox state at its first
  sighting forever, with `Inbox.List()`'s TTL demotion eventually marking a
  genuinely-online device `offline`. Fixed by restarting the whole
  resolve/browse cycle every `rebrowseInterval` (60s, under the Inbox's 90s
  default TTL) instead of browsing once indefinitely.
- Verified against the real device: `ListDiscovery` now reports
  `ready_to_register` once all four fixes were deployed and the device
  reflashed.

**Deployment repair — DeviceAdmin automation update (complete).** Updated 2026-10-01.

- Restored the `admin.Server.UpdateAutomationRule` implementation required by
  the public `apigateway.DeviceAdmin` contract, so the Linux admin binary can
  compile the generated UpdateAutomationRule RPC surface.

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
- The authenticated v2 registration endpoint now accepts only a
  `pairing_required` discovery entry and delegates the whole operation to the
  privileged coordinator; it cannot create a pending binding on its own.
- Added the DNS-SD/mDNS adapter for `_iot-device._tcp.local`; it parses the
  contracted TXT fields into untrusted sightings and inspects each device
  before the inbox exposes it as eligible for pairing.
- `runAdmin` now composes the inbox, mDNS browser, authenticated v2 endpoint
  and secure registration coordinator. Registration is deliberately exposed
  only when DynSec and a LAN-reachable MQTT host are configured.
- Next integration step is a real-device end-to-end run, then connecting the
  C core to each firmware's HTTP, crypto, storage and MQTT adapters.

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

## 2026-10-02 — IPv6 transport for secure registration

- Real DNS-SD discovery on the Orange Pi reaches the ESP32-C3 through IPv6.
  `HTTPInspector` and `SessionClient` now build device URLs with
  `net.JoinHostPort`, so an IPv6 announcement uses a valid authority such as
  `http://[fe80::1]:8080/v1/pair`.
- Added a focused regression test for that authority formatting. This applies
  to device-info inspection, pairing and encrypted provisioning alike.
- Verified with `go test ./internal/devicev2 -count=1`.
