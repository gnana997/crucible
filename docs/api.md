# HTTP API

crucible is driven entirely over HTTP. This is the reference for the daemon's REST surface.

- **Base URL:** whatever you pass to `--listen` (default `http://127.0.0.1:7878`).
- **Authentication.** Off by default on loopback; required as soon as any API key exists. Pass `Authorization: Bearer <key>`. See [Authentication](#authentication) below and [SECURITY.md](../SECURITY.md).
- **Content type:** requests and responses are JSON, except the exec stream (see below). Request bodies are size-capped; oversized bodies are rejected.
- **Errors:** any non-2xx JSON response has the shape `{"error": "message"}`.

IDs are validated on every path; a malformed sandbox or snapshot ID returns `400` before any work happens.

## Authentication

The daemon supports bearer-token API keys, on the same model as the Docker or Kubernetes CLIs.

- **Loopback default: no auth.** With no keys configured, the daemon (bound to `127.0.0.1`) serves every request unauthenticated — the single-operator local case.
- **Enabling auth.** Create a key with `crucible daemon token add --name <label>`. The raw key is printed **once** (`crucible_…`); only its SHA-256 hash is stored. As soon as one key exists, every request must carry `Authorization: Bearer <key>` or it is rejected `401` with a `WWW-Authenticate: Bearer` header. Manage keys with `crucible daemon token list` / `revoke <id>` (rotate = add a new key, then revoke the old); changes take effect without a daemon restart.
- **`/healthz` is always exempt**, so liveness probes work without a key.
- **Binding non-loopback requires auth + TLS.** The daemon refuses to bind a non-loopback address unless at least one key exists and `--tls-cert`/`--tls-key` are set. Clients send the key over TLS with `--token` / `CRUCIBLE_TOKEN`.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness check |
| `GET` | `/metrics` | Prometheus metrics |
| `GET` | `/profiles` | List configured rootfs profiles |
| `POST` | `/sandboxes` | Create a sandbox |
| `GET` | `/sandboxes` | List sandboxes |
| `GET` | `/sandboxes/{id}` | Inspect a sandbox |
| `DELETE` | `/sandboxes/{id}` | Destroy a sandbox |
| `POST` | `/sandboxes/{id}/exec` | Run a command (streamed frames). Add `?stdin=1` for an **interactive**, full-duplex session — the connection is hijacked and the client sends `FrameStdin`/`FrameStdinClose` (what `crucible shell` / `exec -i` use). |
| `POST` | `/sandboxes/{id}/files` | Push a **tar** stream into the guest, extracted beneath `?path=<dest>` (the `crucible cp` push / MCP `write_files`). Streamed, not buffered; entries that escape the dest are rejected. Returns `{files, bytes}`. |
| `GET` | `/sandboxes/{id}/files` | Read one guest file's bytes: `?path=<file>&max_bytes=<n>` (the MCP `read_file`). Content only — nothing is written host-side, so there is no traversal surface. |
| `GET` | `/sandboxes/{id}/logs` | Durable per-sandbox logs (entrypoint output + exec activity). Query: `source=service\|exec\|all`, `since=<byte-cursor>`, tail bounds. `501` when the daemon has no log store configured. |
| `PUT` | `/sandboxes/{id}/service` | Configure the supervised entrypoint (spec). |
| `POST` | `/sandboxes/{id}/service/start\|stop\|restart` | Start / graceful-stop (StopSignal + grace) / restart the entrypoint. |
| `GET` | `/sandboxes/{id}/service` | Entrypoint supervisor status. |
| `GET` | `/sandboxes/{id}/service/logs` | Cursored read of the supervisor's in-guest output ring (the raw source `logs` durably persists). |
| `POST` | `/sandboxes/{id}/snapshot` | Snapshot a sandbox |
| `GET` | `/snapshots` | List snapshots |
| `GET` | `/snapshots/{id}` | Inspect a snapshot |
| `DELETE` | `/snapshots/{id}` | Delete a snapshot |
| `POST` | `/snapshots/{id}/fork` | Fork N sandboxes from a snapshot |
| `POST` | `/images` | Pull + convert an OCI image into the store (`{"ref": "nginx:alpine"}`). |
| `POST` | `/images/import` | Import a `docker save` archive stream into the store. |
| `GET` | `/images` · `GET`/`DELETE` `/images/{ref}` | List / inspect / remove converted images. |

The image, service, and logs endpoints answer `501` when the daemon is started without the corresponding subsystem (`--image-dir`, a service-capable rootfs, `--log-dir`). Calling a known path with an unsupported method returns `405`.

---

### `GET /healthz`

Returns `200` with `{"status": "ok"}` once the daemon is serving. No side effects.

---

### `GET /metrics`

Prometheus metrics in the standard text exposition format. Label-free today:

| Metric | Type | Meaning |
|---|---|---|
| `sandboxes_created_total` | counter | Sandboxes created (cold boots + forks) |
| `sandboxes_active` | gauge | Sandboxes currently live |
| `fork_duration_seconds` | histogram | End-to-end time to bring up one fork |
| `snapshot_restore_duration_seconds` | histogram | Time for the runner to restore a VM from a snapshot |

Like the rest of the API, `/metrics` is subject to auth when keys are configured (only `/healthz` is exempt) and is loopback-bound by default — scrape it from a colocated agent (with a token if auth is on), not across a trust boundary.

---

### `GET /profiles`

Returns the rootfs profiles the daemon was configured with via `--rootfs-dir`, sorted (empty when none):

```json
{ "profiles": ["base", "node-22", "python-3.12"] }
```

These are the values accepted by the `profile` field of `POST /sandboxes`. See [profiles.md](profiles.md).

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
| `image` | object | Boot from an OCI image instead of a profile: `{"oci": "nginx:alpine"}` (a registry ref or a converted digest). The daemon pulls + converts on a store miss and runs the image's entrypoint as the sandbox's service. Mutually exclusive with `profile`. |
| `pull` | string | Image pull policy when `image.oci` is set: `"missing"` (default — convert/cache on a store miss), `"always"` (re-pull even on a hit), `"never"` (fail if not already converted). Ignored when `image` is unset. |
| `publish` | array | Host→guest port publishes, e.g. `[{"host_port":8080,"guest_port":80}]` (optional `host_ip`, default `protocol` `"tcp"`) — the moral equivalent of `docker run -p`. Requires a NIC; when `network` is absent one is created ingress-published, egress-denied. |
| `disk_bytes` | int | Grow the writable rootfs clone to at least this size (`truncate` + `resize2fs`, before boot). `0`/omitted keeps the image/profile headroom; a value smaller than the clone is a no-op (never shrinks). The shared template is never modified. |

**Network policy.** When `network` is present, `enabled: true` **requires** a non-empty `allowlist`; unrestricted full-internet egress is not offered by design (default-deny is the model). Each allowlist entry is an exact hostname (`pypi.org`) or a single-label wildcard (`*.npmjs.org`); a bare `*` is rejected. `enabled: false` with a populated allowlist is a `400` (inconsistent).

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
- Response frame types: `1` = stdout, `2` = stderr, `3` = exit.
- stdout/stderr data frames arrive as the command runs; the final `exit` frame's payload is the `ExecResult` JSON.

Because the `200` is committed before the command finishes, a failure *after* streaming starts (agent unreachable, dropped connection) is delivered in-band as an exit frame with `exit_code: -1` and a populated `error` — never as an HTTP error code.

**Interactive exec** (`POST /sandboxes/{id}/exec?stdin=1`). The one-shot path above is buffered. For a live shell, the daemon **hijacks** the connection into a full-duplex framed stream after the `200`: the client sends inbound frames `4` = stdin and `5` = stdin-close (EOF), and reads the same stdout/stderr/exit frames back. State (`cd`, env) persists for the life of the long-lived process. This is what `crucible shell <id>` and `sandbox exec -i` use; it is line-buffered with **no PTY**. The one-shot path is unchanged.

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

### `POST /sandboxes/{id}/files` and `GET /sandboxes/{id}/files`

File transfer between host and guest — one-way bulk copy, so no frame protocol.

**Push** (`POST …/files?path=<dest>`): the request body is a **tar** stream. The daemon streams it straight to the guest agent (nothing buffered whole); the agent `MkdirAll`s `<dest>` and extracts each entry beneath it, **rejecting** any entry whose resolved path escapes the destination (absolute paths, `..`, or symlinks pointing outside). Returns `200 {"files":N,"bytes":M}`. Gated as an `exec`-class operation. This backs `crucible cp` (push) and the MCP `write_files` tool.

**Read** (`GET …/files?path=<file>&max_bytes=<n>`): returns the raw bytes of a single guest file (capped by `max_bytes`; a directory is a `400`). This is a **content read** — only bytes flow out, nothing is written host-side, so it has no path-traversal surface. Gated as `read`. It backs the MCP `read_file` tool. Pulling a directory *tree* onto the host is intentionally not offered.

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
