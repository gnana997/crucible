# Wire protocol

The parts of crucible's API that OpenAPI cannot express, specified for SDK
authors in any language. Everything JSON-shaped (request/response bodies,
error envelopes) lives in [`openapi.json`](openapi.json) and is generated
from the Go wire types — this document covers only the **binary frame
protocol** and the **streaming transports** that carry it.

Reference implementation: the [`sdk/wire`](../sdk/wire) Go package (codec)
and [`scripts/wsexec`](../scripts/wsexec) (a minimal WebSocket client that
speaks exactly what a non-Go SDK would). Conformance fixtures:
[`sdks/fixtures`](../sdks/fixtures) — see [Fixtures](#fixtures) below.

## Frames

Exec output (and interactive input) travels as a sequence of
length-prefixed frames. Every frame is one fixed 8-byte header followed by
its payload:

| offset | size | field | encoding |
|---|---|---|---|
| 0 | 1 | frame type | see table below |
| 1 | 3 | reserved | zeroed on write, ignored on read |
| 4 | 4 | payload length | `uint32`, big-endian |
| 8 | *length* | payload | raw bytes |

| type | value | direction | payload |
|---|---|---|---|
| `stdout` | 1 | guest → client | raw output chunk |
| `stderr` | 2 | guest → client | raw output chunk |
| `exit` | 3 | guest → client | JSON `ExecResult` (see the `WireExecResult` schema in openapi.json) |
| `stdin` | 4 | client → guest | raw input chunk (interactive only) |
| `stdin_close` | 5 | client → guest | empty — signals stdin EOF without dropping the connection |

Rules (all pinned by the fixtures):

- **Max payload is 65536 bytes** (`max_payload_size` in the fixture
  manifest). Writers must chunk larger logical writes into consecutive
  frames of the same type; readers must treat consecutive same-type frames
  as one continuous stream — chunk boundaries carry no meaning.
- A **response stream ends with exactly one `exit` frame**, then EOF. A
  stream that ends without one is an error (the command was lost).
- Readers must **reject** a header whose declared length exceeds the max
  payload size, and report truncation (EOF mid-header or mid-payload) as an
  error, never as a clean end of stream.
- Frame type values and the header layout are **frozen** — they travel on
  the wire and non-Go clients hard-code them.
- Keep numeric values and JSON tags exactly as specified; unknown frame
  types should be treated as a protocol error.

The framing is deliberately the same shape Docker uses for its container
attach/logs API.

## One-shot exec — `POST /sandboxes/{id}/exec`

Request: JSON `ExecRequest` body (see openapi.json). Response on success:
`200` with `Content-Type: application/octet-stream`, the body being a frame
stream (`stdout`/`stderr` frames as the command runs, one terminal `exit`
frame). Validation failures respond *before* streaming with a normal JSON
error and 4xx status.

Because the `200` is committed before the command finishes, post-commit
failures (agent unreachable, VM died) are reported **in-band**: the daemon
synthesizes an `exit` frame with `exit_code: -1` and an `error` string. The
framing contract always holds — a client never has to parse a half-JSON,
half-frame body.

## Interactive exec

A full-duplex session (persistent `cwd`/env, live stdin) with the same
frame protocol in both directions. Two transports carry it; **the frame
bytes are identical on both**, so one codec serves everything.

### Transport A — hijacked connection: `POST /sandboxes/{id}/exec?stdin=1`

For clients that own a raw TCP/TLS socket (the Go SDK, the CLI's
`shell`/`exec -i`). The client writes an ordinary HTTP/1.1 request with the
JSON `ExecRequest` body; after the daemon answers with a bare
`HTTP/1.1 200 OK` header block, the connection stops being HTTP: the client
sends `stdin`/`stdin_close` frames and reads `stdout`/`stderr`/`exit`
frames until EOF. Closing the connection kills the guest command.

This is the lowest-overhead path, but it is invisible to `fetch()`-style
HTTP APIs and will not traverse an L7 proxy — hence transport B.

### Transport B — WebSocket: `GET /sandboxes/{id}/exec` + upgrade

The cross-language transport (browser-style HTTP stacks, anything behind a
gateway). Contract:

1. Standard WebSocket upgrade handshake on `GET /sandboxes/{id}/exec`.
   Auth is the usual `Authorization: Bearer <key>` header on the handshake
   request. Pre-upgrade failures are plain HTTP errors on the handshake
   response (`400` bad id, `404` unknown sandbox); a plain GET without an
   upgrade handshake answers `426`.
2. The client's **first message is the JSON `ExecRequest`** (text or
   binary; the daemon parses the payload either way). It must arrive
   within 30 seconds. A request that fails validation closes the socket
   with status `1008` and the reason in the close frame.
3. Everything after is the frame protocol: **the concatenation of binary
   message payloads in each direction is exactly the frame stream from
   transport A.** Frames may split across WebSocket messages and messages
   may pack multiple frames — decode the concatenated byte stream, not
   individual messages.
4. A failure to reach the guest closes with status `1011` + reason. After
   the `exit` frame is delivered the daemon closes with `1000`.

## File transfer — `POST /sandboxes/{id}/files` (push), `GET` (pull)

Not framed. Push streams a **tar archive** as the raw request body
(`?path=` names the guest destination directory); the guest agent extracts
entries beneath it and rejects any entry whose resolved path escapes
(absolute paths, `..`, symlinks pointing outside), then answers with a JSON
`WireFilesPutResult`. Pull (`GET …/files?path=`) returns a single file's
raw bytes. Both are plain HTTP streaming — no special client machinery.

## Fixtures

[`sdks/fixtures`](../sdks/fixtures) contains recorded frame streams
(`*.bin`) plus [`manifest.json`](../sdks/fixtures/manifest.json) describing
every frame in them: type byte, payload length, payload SHA-256, the
payload text when short/printable, and the parsed `ExecResult` for exit
frames. Invalid streams (truncated header/payload, oversize length) are
included with the required failure mode.

An SDK's codec test suite should, with no daemon and no KVM:

1. Parse `manifest.json`, assert its `header` constants match the codec's.
2. For every valid fixture: decode the `.bin`, compare each frame against
   the manifest entries, and require clean EOF after the last one.
3. For every `"invalid": true` fixture: require a decode error, never a
   clean EOF.
4. For `stdin_session.bin` (direction `host_to_guest`): *encode* the frames
   listed in the manifest and require byte-identical output — this checks
   the encoder half.

The fixtures are generated by [`sdks/fixtures/gen`](../sdks/fixtures/gen)
using the real Go codec, and the manifest is derived by re-decoding the
generated bytes — they cannot disagree with the implementation. CI
regenerates them (`make gen-fixtures`) and fails on any diff.
