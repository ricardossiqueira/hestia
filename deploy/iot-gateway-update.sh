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

log() { logger -t iot-gateway-update -- "$*"; }
fail() { log "update skipped: $*"; exit 1; }
run_as_deploy_user() {
  runuser -u "$deploy_user" -- env HOME="$deploy_home" "$@"
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
install -d -m 0750 -o root -g root /usr/local/lib/iot-gateway
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
  log "new revision failed to start; restoring the previous service"
  if [[ "$had_binary" == true ]]; then
    install -m 0755 "$backup_binary" "$binary"
  fi
  if [[ "$had_unit" == true ]]; then
    install -m 0644 "$backup_unit" "$unit"
  fi
  systemctl daemon-reload
  systemctl restart iot-gateway.service || log "rollback could not restart the previous service"
  exit 1
fi
sleep 3
if ! systemctl is-active --quiet iot-gateway.service; then
  log "new revision exited after startup; restoring the previous service"
  if [[ "$had_binary" == true ]]; then
    install -m 0755 "$backup_binary" "$binary"
  fi
  if [[ "$had_unit" == true ]]; then
    install -m 0644 "$backup_unit" "$unit"
  fi
  systemctl daemon-reload
  systemctl restart iot-gateway.service || log "rollback could not restart the previous service"
  exit 1
fi

printf '%s\n' "$current_revision" >"$state_file"
rm -f "$backup_binary" "$backup_unit" "$candidate_binary"
log "deployed revision $current_revision"
