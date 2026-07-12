//go:build unix

package reuseport

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// control sets SO_REUSEPORT on the socket before bind.
func control(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return serr
}
