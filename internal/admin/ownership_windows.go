//go:build windows

package admin

import "os"

// restoreOwnership is a no-op on Windows: this package only ever runs for
// real on the Orange Pi (Linux) - see ownership_unix.go for what this does
// there. Windows only needs to build and test cleanly for local
// development (go.mod/CI target Linux ARM64 for the actual binary).
func restoreOwnership(string, os.FileInfo) {}
