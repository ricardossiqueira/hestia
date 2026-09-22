#!/usr/bin/env bash
set -Eeuo pipefail

# This script is run as root by systemd. Git access and compilation run as the
# unprivileged repository owner; only installation and service control require
# root. Local configuration and credentials are deliberately never overwritten.
repository="/home/orangepi/iot-gateway"
deploy_user="orangepi"
deploy_home="/home/orangepi"
branch="main"
# This must not share the gateway's SQLite directory: the gateway process can
# write there, whereas deployment state is privileged root-owned data.
state_dir="/var/lib/iot-gateway-update"
state_file="$state_dir/deployed-revision"
binary="/usr/local/bin/iot-gateway"
unit="/etc/systemd/system/iot-gateway.service"
# iot-gateway-admin.service's ExecStart points at this SAME $binary (see
# deploy/iot-gateway-admin.service) - since docs/decisions.md ADR-013, it is
# also the only public listener for the local API (internal/apigateway),
# reverse-proxying to the sandboxed process this script already manages.
# Restarting only iot-gateway.service after installing a new binary leaves
# admin running the OLD in-memory copy indefinitely (it has no reason to
# restart on its own) - admin's own :8082 listener then silently disappears
# the moment a release moves the API from one process to the other, exactly
# what happened once in production before this was added. No separate
# backup/install step is needed for admin here: it is the same binary file,
# already covered by backup_binary/had_binary below - restarting it after a
# rollback picks the restored binary back up for free.
admin_unit_name="iot-gateway-admin.service"

log() { logger -t iot-gateway-update -- "$*"; }
fail() { log "update skipped: $*"; exit 1; }
run_as_deploy_user() {
  runuser -u "$deploy_user" -- env HOME="$deploy_home" "$@"
}
run_healthcheck() {
  runuser -u iot-gateway -- "$binary" healthcheck --config /etc/iot-gateway/gateway.yaml
}
# False on an installation that never installed the admin UI at all (it is
# optional - deploy/README.md's "Admin UI" section) - restarting/rolling
# back a unit that was never installed would itself be an error, so every
# admin-related step below is skipped entirely in that case.
admin_service_present() {
  systemctl list-unit-files --no-legend "$admin_unit_name" 2>/dev/null | grep -q "$admin_unit_name"
}

# The updater only calls this after a new binary and unit have been installed.
# A complete previous installation is required for a safe rollback. During an
# initial install there is deliberately nothing to restore, so leave the failed
# service(s) stopped and report that condition explicitly.
rollback_gateway() {
  local reason="$1"
  log "$reason"
  if [[ "$had_binary" != true || "$had_unit" != true ]]; then
    log "rollback unavailable: no complete previous gateway installation; leaving service(s) stopped"
    systemctl stop iot-gateway.service || log "could not stop failed initial service"
    if admin_service_present; then
      systemctl stop "$admin_unit_name" || log "could not stop failed initial admin service"
    fi
    return 1
  fi
  install -m 0755 "$backup_binary" "$binary"
  install -m 0644 "$backup_unit" "$unit"
  systemctl daemon-reload
  systemctl restart iot-gateway.service || log "rollback could not restart the previous service"
  # Same shared binary as above: restoring it and restarting admin brings it
  # back to the previous, presumably-working revision too - no separate
  # admin binary/unit backup exists or is needed.
  if admin_service_present; then
    systemctl restart "$admin_unit_name" || log "rollback could not restart the previous admin service"
  fi
  return 1
}

wait_for_healthy_gateway() {
  local attempt
  for attempt in {1..10}; do
    if run_healthcheck; then
      return 0
    fi
    if (( attempt < 10 )); then
      sleep 1
    fi
  done
  return 1
}

[[ -d "$repository/.git" ]] || fail "repository not found at $repository"
install -d -m 0700 -o root -g root "$state_dir"

exec 9>"$state_dir/update.lock"
flock -n 9 || { log "another update is already running"; exit 0; }

if [[ -n "$(run_as_deploy_user git -C "$repository" status --porcelain --untracked-files=all)" ]]; then
  fail "repository has local changes; refusing to overwrite them"
fi

run_as_deploy_user git -C "$repository" fetch --quiet origin "$branch" || fail "git fetch failed"
remote_revision="$(run_as_deploy_user git -C "$repository" rev-parse "origin/$branch")" || fail "remote branch not found"
current_revision="$(run_as_deploy_user git -C "$repository" rev-parse HEAD)" || fail "cannot read local revision"

run_as_deploy_user git -C "$repository" merge-base --is-ancestor "$current_revision" "$remote_revision" || fail "local revision is not an ancestor of origin/$branch"
if [[ "$current_revision" != "$remote_revision" ]]; then
  run_as_deploy_user git -C "$repository" merge --ff-only "origin/$branch" || fail "fast-forward merge failed"
  current_revision="$(run_as_deploy_user git -C "$repository" rev-parse HEAD)"
fi
[[ "$current_revision" == "$remote_revision" ]] || fail "local revision does not match origin/$branch"

deployed_revision="$(cat "$state_file" 2>/dev/null || true)"
if [[ "$current_revision" == "$deployed_revision" ]]; then
  log "already running revision $current_revision"
  exit 0
fi

candidate="$repository/bin/iot-gateway.candidate"
rm -f "$candidate"
trap 'rm -f "$candidate"' EXIT
run_as_deploy_user go -C "$repository" test ./... || fail "tests failed for $current_revision"
run_as_deploy_user go -C "$repository" vet ./... || fail "vet failed for $current_revision"
run_as_deploy_user go -C "$repository" build -o "$candidate" ./cmd/gateway || fail "build failed for $current_revision"

candidate_binary="/usr/local/lib/iot-gateway/iot-gateway.candidate"
backup_binary="/usr/local/lib/iot-gateway/iot-gateway.previous"
backup_unit="/usr/local/lib/iot-gateway/iot-gateway.service.previous"
# The service account validates the staged binary before it is installed, so it
# needs traverse permission but must not be able to modify this directory.
install -d -m 0750 -o root -g iot-gateway /usr/local/lib/iot-gateway
install -m 0755 "$candidate" "$candidate_binary"
runuser -u iot-gateway -- "$candidate_binary" validate --config /etc/iot-gateway/gateway.yaml || fail "installed configuration is invalid"

had_binary=false
had_unit=false
if [[ -f "$binary" ]]; then
  install -m 0755 "$binary" "$backup_binary"
  had_binary=true
fi
if [[ -f "$unit" ]]; then
  install -m 0644 "$unit" "$backup_unit"
  had_unit=true
fi

install -m 0755 "$candidate_binary" "$binary"
install -m 0644 "$repository/deploy/iot-gateway.service" "$unit"
install -m 0755 "$repository/deploy/iot-gateway-update.sh" /usr/local/sbin/iot-gateway-update
install -m 0644 "$repository/deploy/iot-gateway-update.service" /etc/systemd/system/iot-gateway-update.service
install -m 0644 "$repository/deploy/iot-gateway-update.timer" /etc/systemd/system/iot-gateway-update.timer
systemctl daemon-reload

if ! systemctl restart iot-gateway.service; then
  rollback_gateway "new revision failed to start; restoring the previous binary for both services"
  exit 1
fi
if ! systemctl is-active --quiet iot-gateway.service; then
  rollback_gateway "new revision exited after startup; restoring the previous binary for both services"
  exit 1
fi
if ! wait_for_healthy_gateway; then
  rollback_gateway "new revision did not pass local healthcheck after 10 attempts; restoring the previous binary for both services"
  exit 1
fi

# iot-gateway.service (the sandboxed core: MQTT routing, the local API's
# internal/api half) is confirmed healthy at this point. Now bring the
# admin process onto the SAME binary - it will not do so on its own. A
# failure here rolls back BOTH services together: leaving the core gateway
# on the new revision while admin is stuck (crash-looping, or simply never
# restarted) would silently take down :8081 and :8082 until someone
# notices by hand - exactly the failure mode this whole block exists to
# prevent (see admin_unit_name's comment above).
if admin_service_present; then
  if ! systemctl restart "$admin_unit_name"; then
    rollback_gateway "new revision's admin service failed to restart; restoring the previous binary for both services"
    exit 1
  fi
  if ! systemctl is-active --quiet "$admin_unit_name"; then
    rollback_gateway "new revision's admin service exited after restart; restoring the previous binary for both services"
    exit 1
  fi
fi

printf '%s\n' "$current_revision" >"$state_file"
rm -f "$backup_binary" "$backup_unit" "$candidate_binary"
log "deployed revision $current_revision"
