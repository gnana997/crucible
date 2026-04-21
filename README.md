# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.1-dev](https://img.shields.io/badge/status-v0.1--dev-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Status.** `crucible daemon` boots Firecracker microVMs under jailer (chroot + cgroup v2 quotas), manages their lifecycle over HTTP, runs commands over vsock with stdout/stderr streaming and structured execution records (exit code, rusage, OOM kill), captures snapshots, forks end-to-end (each fork in its own netns with its own DHCP-assigned IP), and enforces per-sandbox default-deny egress with a hostname-based allowlist. Don't use this for anything real.

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
| Pre-baked rootfs profiles (base, python, node, go) | 🔨 planned for v0.1 — CI-published as GitHub Release assets with SHA256 |
| HTTP API — sandbox lifecycle (create / list / get / delete) | ✅ done |
| HTTP API — exec inside sandbox via vsock (streaming stdout/stderr) | ✅ done |
| Sandbox lifetime timeout + per-exec deadline | ✅ done |
| JSON lifecycle logs (`--log-format=json`) | ✅ done |
| Graceful SIGTERM drain of active sandboxes | ✅ done |
| Snapshot — capture state + memory + rootfs | ✅ done |
| HTTP API — snapshot (`POST /sandboxes/{id}/snapshot`, `GET /snapshots`, `DELETE /snapshots/{id}`) | ✅ done |
| HTTP API — fork (`POST /snapshots/{id}/fork?count=N`) | ✅ done |
| Lazy memory loading via `userfaultfd` | 🔨 planned for v0.1 — serve guest page faults from the snapshot's memory file instead of byte-copying on fork (same technique as AWS Lambda SnapStart) |
| Jailer integration (chroot + mount/PID namespaces + privilege drop) | ✅ done (requires `sudo`) |
| Resource quotas — CPU (cpu.max), memory (memory.max), PIDs (pids.max) via cgroup v2 | ✅ done under jailer |
| Startup orphan-chroot reap after a crashed daemon | ✅ done |
| Structured execution record | ✅ done — exit metadata (exit_code, duration_ms, signal, timed_out, oom_killed) + nested `usage` with CPU user/sys ms, peak RSS, major faults, involuntary ctx-switches, I/O bytes |
| IO quotas (cgroup `io.max`) | ⏳ deferred — needs per-host block-device discovery |
| OCI image pull (ghcr.io / private registries → ext4 rootfs) | ⏳ planned — wire contract (`image: {path, oci}`) frozen now |
| Default-deny network + allowlist | ✅ done — per-sandbox netns + nftables egress + hand-rolled DNS proxy; exact-match + `*.domain` wildcards; AAAA records stripped (sandboxes are IPv4-only) |
| Prometheus `/metrics` endpoint | ⏳ planned for v0.1 |
| Install script + systemd unit | ⏳ planned for v0.1 |
| Python SDK | ⏳ deferred to v0.2 — the HTTP API is stable and directly usable from any language |

Full trajectory through v1.0: [docs/ROADMAP.md](docs/ROADMAP.md).

## Try it locally

Requirements:

- Linux host with KVM (x86_64). `ls /dev/kvm` succeeds and is readable.
- Go 1.25+ (to build), plus `fakeroot`, `squashfs-tools`, `e2fsprogs` on the host (to bake the rootfs).
- Firecracker v1.15+ binary, a guest kernel (uncompressed `vmlinux`), and a base rootfs (`.squashfs`). Pull them from [Firecracker's CI bucket](https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/) or see the [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md).
- `iproute2`, `nftables`, and `iptables` in `$PATH` when running the daemon with networking enabled — the daemon shells out to them to create netns / veth pairs, manage nft rules, and insert `FORWARD` ACCEPTs that coexist with Docker's default `FORWARD DROP`. All three are stock on Ubuntu/Debian.

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
  --jail-uid 10000 --jail-gid 10000 \
  --network-egress-iface eth0   # or wlp3s0 / ens1 — whichever reaches the internet
```

With `--jailer-bin` set, every microVM gets its own chroot under `<chroot-base>/firecracker/<id>/root/`, its own mount + PID namespaces, its own cgroup v2 slice (for resource quotas), and firecracker itself runs as the unprivileged `--jail-uid`. This is the pattern AWS Lambda and Fly.io use — see the [upstream jailer docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md) for the mechanics.

With `--network-egress-iface` set, every sandbox created with a `network` block gets its own netns, a `/30` out of the per-daemon pool (`--network-subnet-pool`, default `10.20.0.0/16`), a veth pair bridged to a tap, a per-netns DHCP server, and a per-sandbox nft chain that only permits egress to IPs resolved through the daemon's DNS proxy for allowlisted names. At startup the daemon also installs two `iptables FORWARD ACCEPT` rules scoped to our `vh-+` veth prefix so traffic survives the default `FORWARD DROP` that Docker sets on many hosts. For the data-plane details — why the bridge is L3-less, why AAAA is stripped, why the DHCP responder uses `SO_BINDTODEVICE` — see [docs/network-bringup-journal.md](docs/network-bringup-journal.md).

At startup the daemon reaps any orphan chroots left by a previous run (crashed or killed without clean shutdown), so you don't have to babysit `/srv/jailer` between restarts.

### End-to-end smoke tests

Two harnesses, both run as root (jailer + network need `CAP_SYS_ADMIN` + `CAP_NET_ADMIN`):

- [scripts/smoke_fork.sh](scripts/smoke_fork.sh) — minimal fork correctness check: boot a source VM, write a marker inside the guest, snapshot, fork ×3, verify each fork sees the marker, tear everything down.
- [scripts/smoke_e2e.sh](scripts/smoke_e2e.sh) — 15-test battery covering exec roundtrip, exit codes, exec timeouts, OOM kill, structured rusage, default-deny network, hostname allowlist (allowed / denied / IP-literal / `*.domain` wildcard), snapshot + 5-fork with per-fork networking, orphan reap.

Per-test artifacts land under `/tmp/crucible-smoke-YYYYMMDD-HHMMSS/test-NN-*/` — stdout, stderr, and the parsed exit frame for every step — so you can inspect any failing assertion without rerunning.

For single-issue diagnostics there's also [scripts/debug_dns.sh](scripts/debug_dns.sh), which spins up one sandbox and dumps both guest-side (`ip addr`, `resolv.conf`, `ip neigh`, `getent`) and host-side (`nft list`, `bridge fdb`, netns routing) state in one shot.

### Performance (and why your filesystem matters)

Fork cost is dominated by per-fork copies: the snapshot's memory file and rootfs are cloned into per-fork files. [fsutil.Clone](internal/fsutil/clone.go) prefers `FICLONE` (reflink COW, O(1) in file size) but falls back to `io.Copy` when the filesystem doesn't support reflinks.

**ext4 doesn't support reflinks.** Only XFS with `reflink=1` (default since kernel 5.10's `mkfs.xfs`), btrfs, and f2fs do. If `stat -fc %T <crucible-dir>` returns `ext2/ext3`, every `Clone` is a full byte-copy and `Fork` is bottlenecked on disk write bandwidth.

Two orthogonal paths to cheap fork, both in the v0.1 scope:

1. **Run crucible with `--work-base` on a reflink-capable filesystem** (btrfs, XFS-with-reflink). No code changes; `FICLONE` makes per-fork rootfs copies effectively instantaneous.
2. **`userfaultfd` memory backend.** Firecracker supports `mem_backend: Uffd`: guest page faults are delivered to a userspace handler that serves pages directly from the snapshot's memory file — no memory copy at fork time. Same technique AWS Lambda uses for SnapStart.

Actual latency numbers land here once the sandbox-bench harness is producing reproducible measurements — we'd rather publish no numbers than misleading ones.

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
cmd/crucible/               CLI entry + subcommand wiring (daemon)
cmd/crucible-agent/         guest-side binary (vsock listener, /exec, /network/refresh)
internal/fcapi/             hand-written Firecracker HTTP-over-UDS client
internal/fsutil/            Clone (FICLONE reflink / copy), Move (rename + xdev fallback)
internal/jailer/            argv builder, chroot staging, cleanup, orphan reap
internal/runner/            firecracker + jailer process lifecycle
internal/sandbox/           ID generation + Manager (lifecycle, exec, snapshot, fork, timers)
internal/daemon/            HTTP server, routes, middleware, network adapter
internal/agentwire/         shared protocol (frame format, ExecRequest/Result)
internal/agentapi/          host-side HTTP client over hybrid-vsock UDS
internal/network/           Manager + subnet pool, per-sandbox netns + veth + L3-less bridge + tap, nft base/per-sandbox rules, iptables FORWARD ACCEPT for vh-+
internal/network/dhcp/      per-netns DHCP responder (SO_BINDTODEVICE-pinned to the bridge, no MAC filter so forks work)
internal/network/dnsproxy/  shared DNS proxy (allowlist enforcement, AAAA stripping, nft set updates on each A record)
internal/version/           ldflags-settable build version
scripts/                    rootfs builder, smoke_fork.sh, smoke_e2e.sh, debug_dns.sh
docs/                       VISION.md, ROADMAP.md, network-bringup-journal.md
```

Direct dependencies (kept small on purpose):

- `golang.org/x/sys` — raw Linux syscalls for runner + agent (rusage, setns, SO_BINDTODEVICE)
- `github.com/mdlayher/vsock` — AF_VSOCK listener in the guest agent; we tried rolling our own via `net.FileConn` first, but Go's stdlib doesn't recognize AF_VSOCK sockaddrs
- `github.com/miekg/dns` — DNS message parsing + upstream exchange in the proxy; hand-rolling the wire format would have been a weekend of yak-shaving, and miekg/dns is the stable, widely audited choice

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
