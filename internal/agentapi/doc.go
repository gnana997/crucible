// Package agentapi is the host-side client for the crucible-agent
// running inside each sandbox VM.
//
// # What's on the wire
//
// agentapi speaks HTTP/1.1 to the guest agent, same as the agent's own
// mux (see cmd/crucible-agent). The transport path is the interesting
// part — it has three hops:
//
//  1. Host process → unix-domain socket at <workdir>/vsock.sock. This
//     is the per-sandbox "hybrid vsock" socket Firecracker creates on
//     the host when you configure PUT /vsock.
//  2. Unix socket → Firecracker's vsock device (virtio-vsock). The
//     host-side protocol on top of the UDS is Firecracker's own tiny
//     shim: write "CONNECT <port>\n", read "OK <fd>\n", then you have
//     a raw stream to whatever is listening on that vsock port inside
//     the guest.
//  3. Firecracker → the guest kernel's AF_VSOCK listener (the agent).
//     On the guest side it's a normal AF_VSOCK socket: no network
//     stack, no IP, no routing.
//
// The "CONNECT ... / OK" handshake only applies to host-initiated
// connections. The documentation calls this "hybrid vsock" because the
// host end is UDS and the guest end is AF_VSOCK; Firecracker bridges
// them.
//
// # Client contract
//
// Every Client instance is bound to one sandbox: its UDS path is
// per-VM. Callers are responsible for plumbing the right UDS path
// (sandbox.Manager does this via the runner). Methods on Client are
// safe for concurrent use.
//
// # References
//
// Firecracker hybrid vsock protocol:
//
//	https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/docs/vsock.md
package agentapi
