// Package reuseport opens TCP listeners with SO_REUSEPORT set, so a wildcard
// (0.0.0.0) listener can share a port with a specific-address listener — Linux
// then delivers each connection to the most-specific matching bind. crucible
// uses this so a published host port (0.0.0.0:P, e.g. `-p 80:80`) coexists with
// the app→app networking VIP (10.20.255.254:P), which binds the same port on its
// own host-local address, instead of the two clashing with EADDRINUSE.
//
// SO_REUSEPORT is symmetric — two identical binds would silently load-balance —
// so the port-publish layer keeps its own host-port registry to preserve
// one-owner-per-port; SO_REUSEPORT only enables the wildcard/specific overlap.
package reuseport

import (
	"context"
	"net"
)

// Listen is net.Listen("tcp", addr) with SO_REUSEPORT set where the platform
// supports it. The daemon that uses it is Linux-only; on other platforms it
// falls back to a plain listener (the control hook is a no-op).
func Listen(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: control}
	return lc.Listen(context.Background(), "tcp", addr)
}
