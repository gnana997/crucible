// Package agentwire defines the wire protocol between crucible's daemon
// (host) and the guest agent that runs inside every sandbox.
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
// # Request
//
// POST /exec with ExecRequest as the JSON body. Exactly one command per
// request.
//
// Two modes share this endpoint:
//
//   - One-shot (default): the request body is the JSON ExecRequest and
//     nothing more; the response is the frame stream described below.
//   - Interactive (POST /exec?stdin=1): after the JSON ExecRequest body,
//     the connection is hijacked into a full-duplex framed stream. The
//     host sends FrameStdin / FrameStdinClose frames; the guest replies
//     with FrameStdout / FrameStderr / FrameExit frames as usual. This is
//     what backs a live shell into a running sandbox — a functional (no
//     PTY) session with persistent cwd/env.
//
// # Response
//
// The response body is a sequence of length-prefixed frames. Each frame
// is one header (FrameHeaderSize bytes) followed by a payload:
//
//	offset 0 : frame type (1 byte)      FrameStdout | FrameStderr | FrameExit
//	offset 1 : reserved, zeroed         3 bytes
//	offset 4 : payload size in bytes    uint32, big-endian
//	offset 8 : payload                  size bytes
//
// The stream always ends with exactly one FrameExit frame whose payload is
// an ExecResult JSON object. Framing lets stdout and stderr share a single
// HTTP response body without either one corrupting the other — no delimiter
// escaping needed because sizes are explicit.
//
// This format is deliberately the same shape Docker uses for its container
// attach/logs API. Inbound stdin frames (FrameStdin/FrameStdinClose) use
// the same header layout and are parsed with ReadFrame on the guest side.
package agentwire

// AgentVSockPort is the fixed guest-side vsock port the agent listens on.
// Chosen high enough to avoid conflict with any stock services inside a
// minimal Ubuntu rootfs (<1024 is privileged, <32768 is common).
const AgentVSockPort = 52
