//go:build linux

// AF_VSOCK is Linux-only; build the agent on Linux hosts (or cross-
// compile to linux/amd64 — the target is always a microVM guest).

package main

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// listenVSock opens an AF_VSOCK listener bound to (CID_ANY, port) and
// returns it as a net.Listener. Backed by github.com/mdlayher/vsock.
//
// # Wire-level notes:
//
//   - AF_VSOCK is a socket family alongside AF_INET and AF_UNIX. It uses
//     (CID, port) addressing instead of (IP, port). Host is always
//     CID 2; each guest is assigned a CID at VM-creation time (crucible
//     uses 3 by default).
//   - VMADDR_CID_ANY (0xFFFFFFFF) means "accept connections from any
//     CID" — on the guest, that's the host.
//   - No network stack involved: the transport is a ring buffer shared
//     with the VMM via virtio-vsock.
func listenVSock(port uint32) (net.Listener, error) {
	ln, err := vsock.Listen(port, nil)
	if err != nil {
		return nil, fmt.Errorf("vsock: listen on port %d: %w", port, err)
	}
	return ln, nil
}
