// Package wire defines crucible's public wire contract: the framed exec
// stream and the JSON types for exec, supervised services, and file
// transfer. It is the language-neutral part of the API — every SDK
// (Go, and the generated TS/Python clients) mirrors these shapes, and
// the daemon and the guest agent share them so the contract cannot
// drift between the REST surface and the guest protocol.
//
// The package is pure data plus the frame codec: no dependencies beyond
// the standard library, no behavior beyond structural validation that
// must hold on both ends of the wire (ServiceSpec.Validate/Normalize).
//
// # The exec frame stream
//
// POST /sandboxes/{id}/exec on the daemon (and POST /exec on the guest
// agent) responds with a sequence of length-prefixed frames rather than
// JSON. Each frame is one fixed-width header (FrameHeaderSize bytes)
// followed by a payload:
//
//	offset 0 : frame type (1 byte)      FrameStdout | FrameStderr | FrameExit
//	offset 1 : reserved, zeroed         3 bytes
//	offset 4 : payload size in bytes    uint32, big-endian
//	offset 8 : payload                  size bytes
//
// The stream always ends with exactly one FrameExit frame whose payload
// is an ExecResult JSON object. Framing lets stdout and stderr share a
// single HTTP response body without either one corrupting the other —
// no delimiter escaping needed because sizes are explicit. This format
// is deliberately the same shape Docker uses for its container
// attach/logs API.
//
// Interactive exec (POST …/exec?stdin=1) hijacks the connection into a
// full-duplex framed stream: the client sends FrameStdin /
// FrameStdinClose frames using the same header layout, and the guest
// replies with FrameStdout / FrameStderr / FrameExit frames as usual.
// The one-shot exec path never sends inbound frames.
//
// Keep every numeric constant and JSON tag in this package stable —
// they travel on the wire, and non-Go clients hard-code them.
package wire
