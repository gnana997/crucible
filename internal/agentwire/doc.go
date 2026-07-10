// Package agentwire defines the private half of the wire protocol between
// crucible's daemon (host) and the guest agent that runs inside every
// sandbox: the transport handshake and the host→guest control messages
// that never leave the machine (fork identity refresh, static network
// configuration).
//
// The shared, client-visible half — the exec frame stream and the JSON
// types for exec, supervised services, and file transfer — lives in the
// public sdk/wire package, which both this protocol and the daemon's
// REST API speak. Types here are free to evolve with the agent (it is
// baked into rootfs images and versioned with the daemon); types in
// sdk/wire are frozen wire contract.
//
// # Transport
//
// The daemon speaks HTTP/1.1 to the guest agent. On the host side, we open
// the per-sandbox Firecracker "hybrid vsock" unix socket and speak the
// `CONNECT <port>\n` / `OK <fd>\n` handshake; after that, the connection
// is a raw byte stream to the guest agent's vsock listener. The agent,
// running inside the VM, accepts AF_VSOCK connections on a fixed port
// (AgentVSockPort) and serves a small HTTP mux over them.
//
// In other words: the host uses UDS, the guest uses AF_VSOCK, Firecracker
// bridges the two. From Go's perspective both sides are using net/http
// with a non-default dialer.
//
// # Endpoints
//
// POST /exec (one-shot and ?stdin=1 interactive), PUT /service and the
// service lifecycle routes, and PUT /files all use the sdk/wire shapes.
// POST /identity/refresh and POST /network/configure are private to this
// package.
package agentwire

// AgentVSockPort is the fixed guest-side vsock port the agent listens on.
// Chosen high enough to avoid conflict with any stock services inside a
// minimal Ubuntu rootfs (<1024 is privileged, <32768 is common).
const AgentVSockPort = 52
