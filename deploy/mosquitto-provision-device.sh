#!/usr/bin/env bash
set -Eeuo pipefail

# Automates docs/mosquitto-device-provisioning.md: creates (or rotates) a
# device's own MQTT credential and writes its minimal ACL block, then
# reloads Mosquitto. Run this on the Orange Pi, as root, once per device.
#
# Usage:
#   mosquitto-provision-device.sh <device_id> <topic>...
#   mosquitto-provision-device.sh --remove <device_id>
#
# <topic> is one or more of: telemetry state event command command-result
# (the same suffixes as internal/config's Topics struct / mqtt.md). The
# read/write direction is NOT a flag - it is fixed by which topic you name,
# matching docs/mosquitto-device-provisioning.md's own principle ("nunca use
# readwrite devices/<id>/# como atalho"): `command` is always read (a device
# only ever receives commands), the other four are always write.
#
# Re-running with the same device_id rotates its password (mosquitto_passwd
# updates an existing entry in place) and replaces its ACL block with the
# one just given - safe to use both for first registration and later changes
# to a device's granted topics.
#
# Password/ACL file paths are auto-detected from the live Mosquitto config
# (same grep docs/mosquitto-device-provisioning.md tells you to run by
# hand); override with --password-file/--acl-file if detection is wrong.

password_file=""
acl_file=""
remove=false
device_id=""
topics=()

usage() {
  cat >&2 <<'EOF'
Usage:
  mosquitto-provision-device.sh [--password-file FILE] [--acl-file FILE] <device_id> <topic>...
  mosquitto-provision-device.sh [--password-file FILE] [--acl-file FILE] --remove <device_id>

topic: one or more of: telemetry state event command command-result
EOF
  exit 2
}

log() { echo "$*" >&2; }
fail() { log "error: $*"; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --password-file) password_file="$2"; shift 2 ;;
    --acl-file) acl_file="$2"; shift 2 ;;
    --remove) remove=true; shift ;;
    -h|--help) usage ;;
    --) shift; break ;;
    -*) fail "unknown option: $1" ;;
    *) break ;;
  esac
done

[[ $# -ge 1 ]] || usage
device_id="$1"; shift
[[ "$device_id" =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "device_id must be lowercase alphanumeric with hyphens (got: $device_id)"

if [[ "$remove" == true ]]; then
  [[ $# -eq 0 ]] || fail "--remove takes only <device_id>, no topics"
else
  [[ $# -ge 1 ]] || fail "at least one <topic> is required (telemetry state event command command-result)"
  topics=("$@")
  for topic in "${topics[@]}"; do
    case "$topic" in
      telemetry|state|event|command|command-result) ;;
      *) fail "unknown topic \"$topic\" (must be one of: telemetry state event command command-result)" ;;
    esac
  done
fi

(( EUID == 0 )) || fail "must run as root (edits /etc/mosquitto and reloads the service)"

# ---- Discover the real password/ACL file paths, same grep the doc tells a
# human to run by hand. Falls back to the common defaults with a warning if
# nothing is found, instead of failing hard - the operator may know better.
discover_path() {
  local directive="$1" fallback="$2"
  local found
  found="$(grep -R -hoE "^\s*${directive}\s+\S+" /etc/mosquitto 2>/dev/null | awk '{print $2}' | tail -n1 || true)"
  if [[ -n "$found" ]]; then
    echo "$found"
  else
    log "warning: no '${directive}' directive found under /etc/mosquitto; assuming ${fallback}"
    echo "$fallback"
  fi
}

[[ -n "$password_file" ]] || password_file="$(discover_path password_file /etc/mosquitto/passwd)"
[[ -n "$acl_file" ]] || acl_file="$(discover_path acl_file /etc/mosquitto/acl)"
log "password file: $password_file"
log "acl file:      $acl_file"

# ---- ACL block helpers -----------------------------------------------------
# Removes any existing `user <device_id> ... ` block (up to the next `user `
# line or EOF) from $acl_file, preserving its original mode/owner.
strip_acl_block() {
  [[ -f "$acl_file" ]] || return 0
  local tmp
  tmp="$(mktemp "$(dirname "$acl_file")/.acl.XXXXXX")"
  awk -v target="$device_id" '
    /^user[ \t]+/ { skip = ($2 == target) }
    !skip { print }
  ' "$acl_file" >"$tmp"
  local mode owner group
  mode="$(stat -c '%a' "$acl_file")"
  owner="$(stat -c '%U' "$acl_file")"
  group="$(stat -c '%G' "$acl_file")"
  mv "$tmp" "$acl_file"
  # Best-effort: never let an unresolvable owner/group abort the whole
  # operation (the caller re-applies its own known-good chown/chmod anyway
  # right after a provision; this only matters for --remove).
  chmod "$mode" "$acl_file" 2>/dev/null || true
  chown "$owner:$group" "$acl_file" 2>/dev/null || true
}

reload_mosquitto() {
  if systemctl reload mosquitto.service; then
    log "mosquitto reloaded"
  else
    log "warning: 'systemctl reload mosquitto.service' failed - the installed unit may not support reload."
    log "Review the change, then run 'sudo systemctl restart mosquitto.service' yourself (this briefly drops all connected clients)."
  fi
}

if [[ "$remove" == true ]]; then
  if [[ -f "$password_file" ]] && grep -q "^${device_id}:" "$password_file" 2>/dev/null; then
    mosquitto_passwd -D "$password_file" "$device_id"
    log "removed credential for $device_id"
  else
    log "no existing credential for $device_id in $password_file (nothing to remove)"
  fi
  strip_acl_block
  log "removed ACL block for $device_id"
  reload_mosquitto
  log "done. Also disable $device_id in gateway.yaml and revoke it from any firmware still holding the old password."
  exit 0
fi

# ---- Create or rotate the credential ---------------------------------------
# mosquitto_passwd's interactive prompt reads from /dev/tty directly (not
# stdin), so it cannot be scripted without -b. -b puts the password on the
# command line, briefly visible to `ps` on this host - accepted here because
# the password is generated in-process (never typed/echoed by a human) and
# this only ever runs as root on hardware the operator already controls.
password="$(openssl rand -base64 24)"

install -d -m 0750 -o mosquitto -g mosquitto "$(dirname "$password_file")" 2>/dev/null || true
if [[ ! -f "$password_file" ]]; then
  mosquitto_passwd -c -b "$password_file" "$device_id" "$password"
else
  mosquitto_passwd -b "$password_file" "$device_id" "$password"
fi
chown mosquitto:mosquitto "$password_file" 2>/dev/null || true
chmod 0640 "$password_file"

# ---- Write the ACL block ----------------------------------------------------
strip_acl_block
{
  echo "user $device_id"
  for topic in "${topics[@]}"; do
    if [[ "$topic" == "command" ]]; then
      echo "topic read devices/$device_id/command"
    else
      echo "topic write devices/$device_id/$topic"
    fi
  done
} >>"$acl_file"
# Same ownership/mode Debian's mosquitto package uses for its own config
# files; strip_acl_block already preserved these when the file pre-existed,
# this only matters the first time this exact acl_file is created.
chown mosquitto:mosquitto "$acl_file" 2>/dev/null || true
chmod 0640 "$acl_file"

reload_mosquitto

verify_cmd=""
for topic in "${topics[@]}"; do
  if [[ "$topic" == "command" ]]; then
    verify_cmd="mosquitto_sub -h 127.0.0.1 -p 1883 -u $device_id -P '$password' -t devices/$device_id/command -d"
    break
  fi
done
if [[ -z "$verify_cmd" ]]; then
  first_write_topic="${topics[0]}"
  verify_cmd="mosquitto_pub -h 127.0.0.1 -p 1883 -u $device_id -P '$password' -t devices/$device_id/$first_write_topic -m '{}'"
fi

cat <<EOF

Provisioned "$device_id". Put this in the device's own (gitignored) secrets.h:

  #define MQTT_USERNAME "$device_id"
  #define MQTT_PASSWORD "$password"

Verify the grant (run from another terminal on the same network):

  $verify_cmd

A negative test is worth doing too: the same credential should be refused
for any topic not listed above (docs/mosquitto-device-provisioning.md
section 4). Then finish the checklist in docs/device-onboarding.md (declare
the device in gateway.yaml, validate, confirm subscriptions in the gateway
logs).
EOF
