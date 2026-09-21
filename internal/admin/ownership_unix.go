//go:build !windows

package admin

import (
	"os"
	"syscall"
)

// restoreOwnership re-applies info's owner, group, and permission bits to
// the file at path. Used by writeValidated to defend gateway.yaml's
// root:iot-gateway/0640 install-time ownership (deploy/README.md) across a
// rewrite by this package, which runs as root
// (deploy/iot-gateway-admin.service) while the gateway itself reads the
// file as the unprivileged iot-gateway user (deploy/iot-gateway.service).
// Best-effort: failures here are silently ignored by the caller.
func restoreOwnership(path string, info os.FileInfo) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(stat.Uid), int(stat.Gid))
	}
	_ = os.Chmod(path, info.Mode().Perm())
}
