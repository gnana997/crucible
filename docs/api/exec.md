---
title: Exec and the stream protocol
description: "POST /sandboxes/{id}/exec: run a command and stream framed stdout/stderr, go interactive with ?stdin=1, or use the WebSocket transport from non-Go clients."
openapi: "POST /sandboxes/{id}/exec"
---

Run a command inside the sandbox and stream its output.

| Field | Type | Notes |
|---|---|---|
| `cmd` | string[] | Required, non-empty. `cmd[0]` is PATH-resolved inside the guest. |
| `env` | object | Added to (not replacing) the agent's environment. |
| `cwd` | string | Working directory. Empty = inherit from the agent. |
| `timeout_s` | int | Command deadline. On expiry the agent SIGKILLs the process group and sets `timed_out`. `0` = no agent-side deadline. |

Validation errors (bad JSON, empty `cmd`, unknown sandbox) come back before streaming as a normal `4xx` JSON error. Once validation passes, the daemon sends `200` with `Content-Type: application/octet-stream` and streams a frame protocol:

- Each frame is an 8-byte header (1 type byte, 3 reserved bytes, a big-endian `uint32` payload length) followed by the payload.
- Response frame types: `1` = stdout, `2` = stderr, `3` = exit.
- stdout/stderr frames arrive as the command runs; the final exit frame's payload is the `ExecResult` JSON.

> [!NOTE]
> Because the `200` is committed before the command finishes, a failure after streaming starts is delivered in-band as an exit frame with `exit_code: -1` and a populated `error`, never as an HTTP error code.

## Interactive exec

`POST /sandboxes/{id}/exec?stdin=1` hijacks the connection into a full-duplex framed stream after the `200`: the client sends inbound frames `4` = stdin and `5` = stdin-close (EOF), and reads the same stdout/stderr/exit frames back. State (`cd`, env) persists for the life of the long-lived process. This is what `crucible shell <id>` and `sandbox exec -i` use; it is line-buffered with no PTY.

## Interactive exec over WebSocket

`GET /sandboxes/{id}/exec` with an upgrade handshake serves the same session for clients that cannot hijack a raw connection (fetch-based runtimes, anything behind an L7 proxy). The client's first message is the JSON `ExecRequest`; after that, the concatenated binary message payloads in each direction form exactly the same frame stream as the hijacked path, so one frame codec serves both transports.

> [!WARNING]
> Frames may split across WebSocket messages: decode the concatenated stream, not individual messages.

Pre-upgrade failures (unknown sandbox) are plain HTTP errors on the handshake; post-upgrade validation failures close the socket with status 1008 and a reason. A plain GET without the upgrade handshake answers `426`.

## ExecResult

The exit-frame payload:

```json
{
  "exit_code": 0,
  "duration_ms": 247,
  "signal": "",
  "timed_out": false,
  "oom_killed": false,
  "error": "",
  "usage": {
    "cpu_user_ms": 180,
    "cpu_sys_ms": 40,
    "peak_memory_bytes": 35389440,
    "page_faults_major": 2,
    "context_switches_involuntary": 14,
    "io_read_bytes": 0,
    "io_write_bytes": 0
  }
}
```

- `exit_code` is `-1` when the process was killed by a signal or never started.
- `signal` is the signal name (e.g. `"SIGKILL"`) when the process was signalled; empty on a clean exit.
- `oom_killed` is a best-effort heuristic (SIGKILL not from our timeout plus peak RSS at 95%+ of guest memory), not a precise cgroup reading.
- `usage` is `null` when the agent collected no stats, so callers can tell "no data" from "zeroes". I/O counters are per-process and approximate for sub-100 ms commands.

The frame protocol is specified language-neutrally in [the wire protocol](../wire.md), with conformance fixtures under `sdks/fixtures`.
