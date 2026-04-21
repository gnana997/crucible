# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.1-dev](https://img.shields.io/badge/status-v0.1--dev-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Status.** `crucible daemon` boots Firecracker microVMs, manages their lifecycle over HTTP, runs commands inside them over vsock with stdout/stderr streaming back, and captures snapshots end-to-end. The HTTP surface for fork exists, but fork currently requires jailer-based chroot isolation to avoid a vsock UDS path collision — that's the next commit. Don't use this for anything real.

## Why this exists

AI coding agents write code and they want to run that code — to check compile, run tests, try three approaches in parallel. The options today are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, cost scales with usage), or rolling your own Firecracker stack (months of operational work).

crucible is the fourth option: a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives, sane quotas and observability baked in, defaults tuned for AI-generated code.

Full motivation, design, and FAQ: [docs/VISION.md](docs/VISION.md).

## What works today

| Capability | Status |
|---|---|
| Go module + CLI skeleton | ✅ done |
| Firecracker runner (boot VM from config) | ✅ done |
| Per-sandbox rootfs copy (no shared-writable-rootfs corruption) | ✅ done |
| HTTP API — sandbox lifecycle (create / list / get / delete) | ✅ done |
| HTTP API — exec inside sandbox via vsock (streaming stdout/stderr) | ✅ done |
| Sandbox lifetime timeout + per-exec deadline | ✅ done |
| JSON lifecycle logs (`--log-format=json`) | ✅ done |
| Graceful SIGTERM drain of active sandboxes | ✅ done |
| Snapshot — capture state + memory + rootfs | ✅ done |
| HTTP API — snapshot (`POST /sandboxes/{id}/snapshot`, `GET /snapshots`, `DELETE /snapshots/{id}`) | ✅ done |
| HTTP API — fork (`POST /snapshots/{id}/fork?count=N`) | 🔨 surface in place; end-to-end blocked on jailer (see limitations below) |
| Jailer integration (chroot + cgroup v2 + privilege drop) | 🔨 next — unblocks fork + unlocks resource quotas |
| Structured execution record | 🔨 partial — exit_code, duration_ms, signal, timed_out (no resource stats yet) |
| Resource quotas (CPU / memory / disk / IO) | ⏳ lands with jailer |
| Default-deny network + allowlist | ⏳ planned |
| Prometheus `/metrics` endpoint | ⏳ planned |
| Python SDK | ⏳ planned |
| Install script + systemd unit | ⏳ planned |

### Known limitations (wk3)

**Fork currently fails at the Firecracker layer.** The orchestration is all in place and unit-tested end-to-end against runner stubs, but taking a snapshot and then calling `POST /snapshots/{id}/fork` against real Firecracker yields:

```
VsockUnixBackend: Error binding to the host-side Unix socket: Address in use (os error 98)
```

Firecracker v1.15 captures the vsock device's host UDS path inside the snapshot state file; on `PUT /snapshot/load` it tries to bind that exact path, and it collides with the source's still-running firecracker. Firecracker's `SnapshotLoad` params have `network_overrides` but no `vsock_overrides`, so there's no API-level fix. The canonical production answer is **jailer**, which puts each firecracker in its own chroot so the vsock UDS is always at a fresh `/vsock.sock` inside its jail — snapshot captures that relative path and every fork has its own chroot. Jailer also unlocks host cgroup v2 quotas (CPU / memory / IO / PIDs) as a side effect. It's the next commit.

Full trajectory through v1.0: [docs/ROADMAP.md](docs/ROADMAP.md).

## Try it locally

Requirements:

- Linux host with KVM (x86_64). `ls /dev/kvm` succeeds and is readable.
- Go 1.25+ (to build), plus `fakeroot`, `squashfs-tools`, `e2fsprogs` on the host (to bake the rootfs).
- Firecracker v1.15+ binary, a guest kernel (uncompressed `vmlinux`), and a base rootfs (`.squashfs`). Pull them from [Firecracker's CI bucket](https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/) or see the [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md).

### Build everything

```bash
# Daemon binary
make build

# Guest agent (static linux/amd64 ELF) + an ext4 rootfs with the agent
# baked in and enabled as a systemd service.
make rootfs BASE_ROOTFS=/path/to/ubuntu-24.04.squashfs OUT_ROOTFS=assets/rootfs.ext4
```

The rootfs build uses `fakeroot + mkfs.ext4 -d` under the hood — no sudo needed.

### Start the daemon

```bash
./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --kernel           /path/to/vmlinux \
  --rootfs           assets/rootfs.ext4
# listens on 127.0.0.1:7878 by default
```

### Exercise the API

```bash
# Create a sandbox (body optional; defaults: 1 vCPU, 512 MiB, no timeout).
curl -sS -X POST http://127.0.0.1:7878/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"vcpus": 2, "memory_mib": 512, "timeout_s": 60}'
# → {"id":"sbx_...","vcpus":2,"memory_mib":512,"workdir":"...","created_at":"..."}

# List all
curl -sS http://127.0.0.1:7878/sandboxes

# Run a command inside the sandbox — response body is a stream of framed
# stdout / stderr / exit records; the last frame carries the ExecResult
# (exit_code, duration_ms, signal, timed_out).
curl -sS -X POST http://127.0.0.1:7878/sandboxes/sbx_.../exec \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["/bin/uname","-a"]}' \
  --output /tmp/exec.bin

# Tear down
curl -sS -X DELETE http://127.0.0.1:7878/sandboxes/sbx_...
```

Parse the framed exec output in Python:

```python
import struct, json
with open('/tmp/exec.bin', 'rb') as f:
    while hdr := f.read(8):
        typ, size = hdr[0], struct.unpack('>I', hdr[4:8])[0]
        body = f.read(size)
        name = {1: 'stdout', 2: 'stderr', 3: 'exit'}.get(typ, f'type{typ}')
        print(name, json.loads(body) if typ == 3 else body.decode())
```

Each sandbox gets its own workdir under `--work-base` (default `/tmp/crucible/run/`) containing the Firecracker API socket, the hybrid-vsock UDS, and `firecracker.log` — the guest kernel + userspace serial console streams into that log file, so you can tail it while developing. `Ctrl-C` / `SIGTERM` on the daemon gracefully drains active sandboxes before exiting.

## Development

Build and smoke-test:

```bash
git clone https://github.com/gnana997/crucible
cd crucible
make build
./crucible version
```

Make targets:

```bash
make build    # daemon binary
make agent    # guest agent (static linux/amd64 ELF under bin/)
make rootfs   # bake agent into an ext4 rootfs (needs BASE_ROOTFS=...)
make test     # go test ./...
make race     # with -race
make vet      # go vet
make fmt      # gofmt -s -w .
make lint     # golangci-lint run  (requires golangci-lint installed)
make tidy     # go mod tidy
make clean    # rm built binaries
```

Repository layout:

```
cmd/crucible/         CLI entry + subcommand wiring (daemon)
cmd/crucible-agent/   guest-side binary (vsock listener + /exec handler)
internal/fcapi/       hand-written Firecracker HTTP-over-UDS client
internal/runner/      firecracker process lifecycle
internal/sandbox/     ID generation + Manager (lifecycle, exec, timers)
internal/daemon/      HTTP server, routes, middleware
internal/agentwire/   shared protocol (frame format, ExecRequest/Result)
internal/agentapi/    host-side HTTP client over hybrid-vsock UDS
internal/version/     ldflags-settable build version
scripts/              rootfs builder
docs/                 VISION.md + ROADMAP.md
```

Direct dependencies (kept small on purpose):

- `golang.org/x/sys` — raw Linux syscalls for runner + agent
- `github.com/mdlayher/vsock` — AF_VSOCK listener in the guest agent; we tried rolling our own via `net.FileConn` first, but Go's stdlib doesn't recognize AF_VSOCK sockaddrs

Everything else (HTTP, JSON, concurrency, Firecracker API, host-side hybrid-vsock handshake, frame protocol) is stdlib + hand-written. See [docs/VISION.md](docs/VISION.md) for the rationale.

CI runs `go vet`, `gofmt` check, `-race` tests, `go build`, and `golangci-lint` on every push and PR.

## Roadmap at a glance

- **v0.1** core runtime: Firecracker orchestration, HTTP API, snapshot/fork, quotas, observability primitives, Python SDK
- **v0.2** policy files, language profiles, custom rootfs builder, DNS filtering
- **v0.3** full OpenTelemetry, syscall tracing, record + replay
- **v0.4** fork trees with pruning and scoring
- **v0.5** multi-tenant mode, API keys, audit log
- **v0.6** Kubernetes operator
- **v0.7** distributed scheduler across a fleet of hosts
- **v0.8** GPU passthrough (likely via Cloud Hypervisor)
- **v0.9** streaming exec + interactive stdin
- **v1.0** stability commitment, security audit, first-party integrations (Claude Code, Cursor, LangChain, MCP)

Details in [docs/ROADMAP.md](docs/ROADMAP.md).

## Contributing

Too early. Nothing functional to contribute to yet. Once v0.1 lands, the "please PR" list will include additional language profiles, per-language seccomp tuning, and integrations with specific agent frameworks.

If you're building a coding agent and want crucible to fit your use case, open an issue with your workflow. Concrete use cases shape the roadmap more than abstract wishlist items.

## License

Apache License 2.0. See [LICENSE](LICENSE).

---

*crucible is a working name. If a better one emerges during v0.1, it'll change before v1.0.*
