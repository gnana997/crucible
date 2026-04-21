# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.1-dev](https://img.shields.io/badge/status-v0.1--dev-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Status.** `crucible daemon` boots Firecracker microVMs, manages their lifecycle over HTTP, runs commands inside them over vsock with stdout/stderr streaming back, captures snapshots, and forks them end-to-end under jailer with cgroup v2 quotas. Don't use this for anything real.

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
| HTTP API — fork (`POST /snapshots/{id}/fork?count=N`) | ✅ done |
| Jailer integration (chroot + mount/PID namespaces + privilege drop) | ✅ done (requires `sudo`) |
| Resource quotas — CPU (cpu.max), memory (memory.max), PIDs (pids.max) via cgroup v2 | ✅ done under jailer |
| Startup orphan-chroot reap after a crashed daemon | ✅ done |
| Structured execution record | ✅ done — exit metadata (exit_code, duration_ms, signal, timed_out, oom_killed) + nested `usage` with CPU user/sys ms, peak RSS, major faults, involuntary ctx-switches, I/O bytes |
| IO quotas (cgroup `io.max`) | ⏳ deferred — needs per-host block-device discovery |
| OCI image pull (ghcr.io / private registries → ext4 rootfs) | ⏳ planned — wire contract (`image: {path, oci}`) frozen now |
| Default-deny network + allowlist | ⏳ planned |
| Prometheus `/metrics` endpoint | ⏳ planned |
| Python SDK | ⏳ planned |
| Install script + systemd unit | ⏳ planned |

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

Development mode — direct firecracker launch, no sudo, no jailer, no cgroup quotas:

```bash
./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --kernel           /path/to/vmlinux \
  --rootfs           assets/rootfs.ext4
# listens on 127.0.0.1:7878 by default
```

Production-style mode — jailer chroot + cgroup v2 quotas + privilege drop. Requires root because jailer needs `CAP_SYS_ADMIN` to unshare namespaces, mknod `/dev/kvm`, and chown files before it can drop to an unprivileged uid:

```bash
sudo ./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --jailer-bin      /path/to/jailer \
  --kernel          /path/to/vmlinux \
  --rootfs          assets/rootfs.ext4 \
  --chroot-base     /srv/jailer \
  --jail-uid 10000 --jail-gid 10000
```

With `--jailer-bin` set, every microVM gets its own chroot under `<chroot-base>/firecracker/<id>/root/`, its own mount + PID namespaces, its own cgroup v2 slice (for resource quotas), and firecracker itself runs as the unprivileged `--jail-uid`. This is the pattern AWS Lambda and Fly.io use — see the [upstream jailer docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md) for the mechanics.

At startup the daemon reaps any orphan chroots left by a previous run (crashed or killed without clean shutdown), so you don't have to babysit `/srv/jailer` between restarts.

### End-to-end smoke test

After you have a jailer-capable environment wired up, [scripts/smoke_fork.sh](scripts/smoke_fork.sh) drives the full flow: boot a source VM, write a marker inside the guest, snapshot, fork ×3, verify each fork sees the marker, tear everything down. Run as root.

### Performance (and why your filesystem matters)

Measured latencies against Firecracker v1.15 under jailer, 512 MiB guest, 1 GB rootfs, on an ext4 NVMe host:

| Stage | Latency |
|---|---|
| Source cold boot (incl. guest agent ready) | ~4.5s |
| Snapshot (pause → clone rootfs → write state+mem → resume) | ~4.4s |
| Fork ×3 (parallel goroutines, ext4) | ~6.5s total |
| Teardown | <300ms |

**Fork is disk-bound on ext4.** Each fork byte-copies ~1.5 GB (1 GB rootfs + 512 MiB memory) into per-fork files. Three forks = ~4.5 GB of writes to the same physical disk, at ~700 MB/s effective throughput — about as fast as the NVMe will go. Parallelizing the goroutines doesn't help *here*: the goroutines run concurrently but they're all waiting on the same disk. We keep the parallelism anyway because it does pay off once the filesystem stops being the bottleneck.

[fsutil.Clone](internal/fsutil/clone.go) prefers `FICLONE` (reflink COW, O(1) in file size) but falls back to `io.Copy` when the filesystem doesn't support reflinks. **ext4 doesn't.** Only XFS with `reflink=1` (default since kernel 5.10's `mkfs.xfs`), btrfs, and f2fs do. If `stat -fc %T <crucible-dir>` returns `ext2/ext3`, every `Clone` is a full byte-copy.

Two roads to sub-second fork:

1. **Put `--work-base` on a reflink-capable filesystem** (btrfs loopback, or an XFS-reflink partition). No code changes; `FICLONE` makes per-fork copies effectively instantaneous, and the parallelism in `Fork` starts showing real speedup because the non-I/O work (jailer spawn, `LoadSnapshot`, vCPU restore) is all that's left.
2. **`userfaultfd` memory backend** (v0.2 target). Firecracker supports `mem_backend: Uffd` which lazy-loads guest memory pages on demand from a shared source file via a userspace page-fault handler — no memory copy at fork time at all. This is the technique AWS Lambda uses; done right it drops fork latency to the 100–300 ms range regardless of filesystem.

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
# stdout / stderr / exit records; the last frame carries an ExecResult
# JSON payload with exit_code, duration_ms, signal, timed_out,
# oom_killed, and a nested `usage` object (CPU user/sys ms, peak RSS,
# major page faults, involuntary context switches, I/O bytes
# read/written). Usage is populated from wait4's Rusage plus a
# /proc/<pid>/io poller running alongside the child.
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
internal/fsutil/      Clone (FICLONE reflink / copy), Move (rename + xdev fallback)
internal/jailer/      argv builder, chroot staging, cleanup, orphan reap
internal/runner/      firecracker + jailer process lifecycle
internal/sandbox/     ID generation + Manager (lifecycle, exec, snapshot, fork, timers)
internal/daemon/      HTTP server, routes, middleware
internal/agentwire/   shared protocol (frame format, ExecRequest/Result)
internal/agentapi/    host-side HTTP client over hybrid-vsock UDS
internal/version/     ldflags-settable build version
scripts/              rootfs builder, smoke_fork.sh (end-to-end jailer test)
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
