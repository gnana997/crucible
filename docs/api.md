# HTTP API

crucible is driven entirely over HTTP. This is the `v0.1` reference for the daemon's REST surface.

- **Base URL:** whatever you pass to `--listen` (default `http://127.0.0.1:7878`).
- **No authentication.** The daemon binds loopback by default and has no access control — do not expose it to untrusted callers. See [SECURITY.md](../SECURITY.md).
- **Content type:** requests and responses are JSON, except the exec stream (see below). Request bodies are size-capped; oversized bodies are rejected.
- **Errors:** any non-2xx JSON response has the shape `{"error": "message"}`.

IDs are validated on every path; a malformed sandbox or snapshot ID returns `400` before any work happens.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness check |
| `GET` | `/metrics` | Prometheus metrics |
| `POST` | `/sandboxes` | Create a sandbox |
| `GET` | `/sandboxes` | List sandboxes |
| `GET` | `/sandboxes/{id}` | Inspect a sandbox |
| `DELETE` | `/sandboxes/{id}` | Destroy a sandbox |
| `POST` | `/sandboxes/{id}/exec` | Run a command (streamed) |
| `POST` | `/sandboxes/{id}/snapshot` | Snapshot a sandbox |
| `GET` | `/snapshots` | List snapshots |
| `GET` | `/snapshots/{id}` | Inspect a snapshot |
| `DELETE` | `/snapshots/{id}` | Delete a snapshot |
| `POST` | `/snapshots/{id}/fork` | Fork N sandboxes from a snapshot |

Calling a known path with an unsupported method returns `405`.

---

### `GET /healthz`

Returns `200` with `{"status": "ok"}` once the daemon is serving. No side effects.

---

### `GET /metrics`

Prometheus metrics in the standard text exposition format. Label-free in `v0.1`:

| Metric | Type | Meaning |
|---|---|---|
| `sandboxes_created_total` | counter | Sandboxes created (cold boots + forks) |
| `sandboxes_active` | gauge | Sandboxes currently live |
| `fork_duration_seconds` | histogram | End-to-end time to bring up one fork |
| `snapshot_restore_duration_seconds` | histogram | Time for the runner to restore a VM from a snapshot |

Like the rest of the API this endpoint is unauthenticated and loopback-bound — scrape it from a colocated agent, not across a trust boundary.

---

### `POST /sandboxes`

Create a sandbox. **All fields are optional** — an empty body `{}` boots a sandbox with daemon defaults.

```json
{
  "vcpus": 1,
  "memory_mib": 512,
  "boot_args": "",
  "timeout_s": 0,
  "network": {
    "enabled": true,
    "allowlist": ["pypi.org", "*.npmjs.org"]
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `vcpus` | int | vCPU count. Defaulted and range-capped by the daemon. |
| `memory_mib` | int | Guest memory in MiB. Defaulted and range-capped. |
| `boot_args` | string | Extra kernel cmdline appended to the daemon's default. |
| `timeout_s` | int | Max sandbox lifetime in seconds. `0` = no timeout (lives until `DELETE` or shutdown). |
| `profile` | string | Pre-baked rootfs to boot from, e.g. `"python-3.12"`, `"node-22"`, `"base"`. Empty uses the daemon's default `--rootfs`. Resolved against the daemon's `--rootfs-dir`; an unknown profile is a `400`. See [profiles.md](profiles.md). |
| `network` | object | Omit or `null` for **no network** (default-deny). See below. |
| `image` | object | Reserved wire field. Any value returns `501` in `v0.1` — leave unset and use the daemon's `--rootfs`. |

**Network policy.** When `network` is present, `enabled: true` **requires** a non-empty `allowlist`; full-internet egress is not offered in `v0.1`. Each allowlist entry is an exact hostname (`pypi.org`) or a single-label wildcard (`*.npmjs.org`); a bare `*` is rejected. `enabled: false` with a populated allowlist is a `400` (inconsistent).

**Response** `201 Created`:

```json
{
  "id": "sbx_7k2m...",
  "vcpus": 1,
  "memory_mib": 512,
  "workdir": "/tmp/crucible/run/sbx_7k2m...",
  "created_at": "2026-01-01T00:00:00Z",
  "network": {
    "enabled": true,
    "guest_ip": "10.20.0.2",
    "gateway": "10.20.0.1",
    "allowlist": ["pypi.org", "*.npmjs.org"]
  }
}
```

`network` is omitted when the sandbox has no NIC; `profile` is echoed back when the sandbox booted from a named profile. Errors: `400` (invalid JSON or config, including an unknown `profile`), `501` (`image` set), `500` (boot failure).

---

### `GET /sandboxes`

Returns `200` with `{"sandboxes": [ <sandbox>, ... ]}` (empty array when none). Each element has the same shape as the create response.

### `GET /sandboxes/{id}`

Returns `200` with the sandbox object, `404` if unknown, `400` on a malformed ID.

### `DELETE /sandboxes/{id}`

Tears down the VM and reclaims its resources. Returns `204 No Content` on success, `404` if unknown.

---

### `POST /sandboxes/{id}/exec`

Run a command inside the sandbox and **stream** its output. Request body:

```json
{
  "cmd": ["pytest", "/app/tests"],
  "env": { "CI": "1" },
  "cwd": "/app",
  "timeout_s": 120
}
```

| Field | Type | Notes |
|---|---|---|
| `cmd` | string[] | **Required, non-empty.** `cmd[0]` is PATH-resolved inside the guest. |
| `env` | object | Added to (not replacing) the agent's environment. |
| `cwd` | string | Working directory. Empty = inherit from the agent. |
| `timeout_s` | int | Command deadline. On expiry the agent SIGKILLs the process group and sets `timed_out`. `0` = no agent-side deadline. |

Validation errors (bad JSON, empty `cmd`, unknown sandbox) come back **before** streaming as a normal `4xx` JSON error. Once validation passes the daemon sends `200` with `Content-Type: application/octet-stream` and streams a **frame protocol**:

- Each frame = an 8-byte header (1 type byte, 3 reserved bytes, a big-endian `uint32` payload length) followed by the payload.
- Frame types: `1` = stdout, `2` = stderr, `3` = exit.
- stdout/stderr data frames arrive as the command runs; the final `exit` frame's payload is the `ExecResult` JSON.

Because the `200` is committed before the command finishes, a failure *after* streaming starts (agent unreachable, dropped connection) is delivered in-band as an exit frame with `exit_code: -1` and a populated `error` — never as an HTTP error code.

**`ExecResult`** (the exit-frame payload):

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
- `oom_killed` is a best-effort heuristic (SIGKILL not from our timeout + peak RSS ≥ 95% of guest memory), not a precise cgroup reading.
- `usage` is `null` when the agent collected no stats (e.g. the process never started), so callers can tell "no data" from "zeroes". I/O counters are per-process and approximate for sub-100 ms commands (see the field docs in `internal/agentwire/messages.go`).

---

### `POST /sandboxes/{id}/snapshot`

Pause the sandbox and write a snapshot (a VM state file + a guest-memory file) to disk. Returns `201` with:

```json
{
  "id": "snp_...",
  "source_id": "sbx_7k2m...",
  "vcpus": 1,
  "memory_mib": 512,
  "dir": "/tmp/crucible/run/.../snap",
  "state_path": ".../state",
  "mem_path": ".../mem",
  "rootfs_path": ".../rootfs.ext4",
  "created_at": "2026-01-01T00:00:00Z"
}
```

The on-disk paths are returned for operator debugging (`ls`, `du`, `debugfs`). Errors: `404` (unknown sandbox), `500`.

### `GET /snapshots` / `GET /snapshots/{id}` / `DELETE /snapshots/{id}`

List (`{"snapshots": [...]}`), inspect (`200` / `404`), and delete (`204` / `404`) snapshots. Same object shape as the snapshot response above.

---

### `POST /snapshots/{id}/fork`

Create sandboxes from a snapshot. Fan-out is set by the `?count=N` query parameter (default `1`); `N` must be a positive integer and is capped by the daemon's max fork count (`400` if exceeded).

Fork is **all-or-nothing**: if any child fails to come up, every child started so far is rolled back and the call returns an error. On success, returns `201`:

```json
{ "sandboxes": [ <sandbox>, <sandbox>, ... ] }
```

Each child is a fully independent sandbox (its own ID, network, and — thanks to clone-safety — its own fresh RNG state and machine identifiers). Errors: `400` (bad `count` or invalid config), `404` (unknown snapshot), `500`.

---

## Example session

```bash
# create
SBX=$(curl -s -XPOST localhost:7878/sandboxes -d '{"memory_mib":1024}' | jq -r .id)

# run setup
curl -s -XPOST localhost:7878/sandboxes/$SBX/exec \
  -d '{"cmd":["sh","-lc","git clone https://github.com/x/y /app && cd /app && pip install -r requirements.txt"]}' --output -

# snapshot after setup
SNP=$(curl -s -XPOST localhost:7878/sandboxes/$SBX/snapshot | jq -r .id)

# fork 4 children from the warm snapshot
curl -s -XPOST "localhost:7878/snapshots/$SNP/fork?count=4" | jq '.sandboxes[].id'
```

The exec response is a binary frame stream; a real client parses the frames (see `internal/agentwire` for the reference reader/writer). The Python SDK on the roadmap wraps all of this.
