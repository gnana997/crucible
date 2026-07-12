//go:build !unix

package reuseport

import "syscall"

// control is a no-op where SO_REUSEPORT is unavailable (e.g. Windows). The
// daemon that binds these listeners is Linux-only, so this path is never taken
// at runtime — it exists only so the single cross-platform binary compiles.
func control(_, _ string, _ syscall.RawConn) error { return nil }
