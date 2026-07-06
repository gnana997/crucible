# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.1-dev](https://img.shields.io/badge/status-v0.1--dev-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Status.** `crucible daemon` boots Firecracker microVMs under jailer (chroot + mount/PID namespaces + privilege drop), manages their lifecycle over HTTP, runs commands over vsock with streamed stdout/stderr and structured execution records (exit code, rusage, OOM kill), captures snapshots, and forks end-to-end — each fork restored with **lazy `userfaultfd` memory** (no per-fork RAM copy), its own netns with a DHCP-assigned IP, per-sandbox default-deny egress behind a hostname allowlist, and a per-fork **identity refresh** so no two forks wake with the same kernel RNG state, machine-id, or hostname. Registries are persisted and reconciled on restart. It's `v0.1` — don't run this against anything you can't afford to lose, and don't expose it to untrusted callers yet (see [SECURITY.md](SECURITY.md)).

## Why this exists

AI coding agents write code and they want to run that code — to check it compiles, run the tests they just wrote, try three approaches in parallel. The options today are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, cost scales with usage), or rolling your own Firecracker stack (months of operational work).

crucible is the fourth option: a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives, sane defaults, and observability baked in — tuned for AI-generated code.

Full motivation, design, and FAQ: [docs/VISION.md](docs/VISION.md).

## What works today

| Capability | Status |
|---|---|
| Go module + CLI (`crucible daemon`, `crucible version`) | ✅ done |
| Firecracker runner (boot a microVM from config) | ✅ done |
| Jailer integration (chroot + mount/PID namespaces + privilege drop) | ✅ done (requires `sudo`) |
| Per-sandbox rootfs copy (no shared-writable-rootfs corruption) | ✅ done |
| HTTP API — sandbox lifecycle (create / list / get / delete) | ✅ done |
| HTTP API — exec via vsock (streamed stdout/stderr, structured exit record) | ✅ done |
| HTTP API — snapshot (`POST /sandboxes/{id}/snapshot`, `GET`/`DELETE /snapshots/{id}`) | ✅ done |
| HTTP API — fork (`POST /snapshots/{id}/fork?count=N`) | ✅ done |
| **Lazy memory via `userfaultfd`** — serve guest page faults from the snapshot's memory file instead of byte-copying RAM on fork (same technique as AWS Lambda SnapStart; filesystem-independent) | ✅ done (`internal/memfault`) |
| **Clone-safety** — per-fork identity refresh: kernel CRNG reseed (host entropy) + `/etc/machine-id`/hostname rotation, ordered before the fork is execable so no two forks share RNG/UUIDs | ✅ done |
| **Durable registry + reconcile-on-restart** — sandbox/snapshot records journaled; on startup snapshots are re-adopted and orphaned sandbox state (workdirs, netns, nft, processes) is reaped | ✅ done |
| Default-deny per-sandbox network + hostname allowlist | ✅ done — per-sandbox netns + veth + tap + nftables; egress only to IPs resolved through the daemon's DNS proxy for allowlisted names |
| Network egress hardening | ✅ done — resolved-IP range filter (blocks link-local / RFC1918 / CGNAT so guests can't SSRF cloud metadata), an nft `input` chain with per-sandbox source anti-spoofing, and DNS-layer concurrency + rate limiting |
| Per-request resource ceilings (max vCPUs, memory, fork count) | ✅ done — enforced at the API boundary |
| Sandbox lifetime timeout + per-exec deadline | ✅ done |
| Structured execution record | ✅ done — `exit_code`, `duration_ms`, `signal`, `timed_out`, `oom_killed` + nested `usage` (CPU user/sys ms, peak RSS, major faults, involuntary ctx-switches, I/O bytes) |
| JSON lifecycle logs (`--log-format=json`) + graceful SIGTERM drain | ✅ done |
| Host-side cgroup v2 quotas (cpu.max / memory.max / pids.max) under jailer | ✅ on by default — sized per sandbox from its vCPU/memory request (`--cgroup-quotas=off` to disable) |
| Native language rootfs profiles (base, python, node, go — versioned) | ✅ built from official language images via `make profile PROFILE=…`; selected with the create `profile` field |
| Prometheus `/metrics` endpoint | ✅ `sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds` |
| Install script + systemd unit | ⏳ planned for v0.1 |
| OCI image pull (ghcr.io / private registries → ext4 rootfs) | ⏳ planned — wire contract (`image: {path, oci}`) frozen now; both return `501` in v0.1 |
| Python SDK | ⏳ deferred — the HTTP API is stable and usable from any language |

## Try it locally

Requirements:

- Linux host with KVM (x86_64). `ls /dev/kvm` succeeds and is readable.
- Go 1.25+ (to build), plus `fakeroot`, `squashfs-tools`, `e2fsprogs` (to bake the rootfs).
- Firecracker **v1.15+** binary, a guest kernel (uncompressed `vmlinux`), and a base rootfs (`.squashfs`). Pull them from [Firecracker's CI bucket](https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/) or the [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md).
- `iproute2`, `nftables`, `iptables` in `$PATH` when running with networking — the daemon shells out to them to build netns / veth pairs, manage nft rules, and add `FORWARD` ACCEPTs that coexist with Docker's default `FORWARD DROP`. Stock on Ubuntu/Debian.
- For lazy (`userfaultfd`) fork under jailer, set `vm.unprivileged_userfaultfd=1` (persist via `sysctl.d`) — jailed firecracker runs unprivileged and needs it to register the guest-memory uffd.

### Build

```bash
make build          # daemon binary
make agent          # guest agent (static linux/amd64 ELF under bin/)
make rootfs BASE_ROOTFS=/path/to/ubuntu-24.04.squashfs OUT_ROOTFS=assets/rootfs.ext4
```

`make rootfs` bakes the agent into an ext4 image and enables it as a systemd service, using `fakeroot + mkfs.ext4 -d` — no sudo needed.

### Run the daemon

Development mode — direct firecracker launch, no sudo, no jailer:

```bash
./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --kernel          /path/to/vmlinux \
  --rootfs          assets/rootfs.ext4
# listens on 127.0.0.1:7878 by default
```

Production-style mode — jailer chroot + privilege drop (needs root for `CAP_SYS_ADMIN`), plus per-sandbox networking:

```bash
sudo ./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --jailer-bin      /path/to/jailer \
  --kernel          /path/to/vmlinux \
  --rootfs          assets/rootfs.ext4 \
  --chroot-base     /srv/jailer \
  --jail-uid 10000 --jail-gid 10000 \
  --network-egress-iface eth0   # whichever iface reaches the internet
```

With `--jailer-bin`, every microVM gets its own chroot under `<chroot-base>/firecracker/<id>/root/`, its own mount + PID namespaces, and firecracker runs as the unprivileged `--jail-uid`. **Fork is supported only in jailer mode** — on Firecracker v1.15 the direct (non-jailer) restore path cannot rewire vsock after load, so jailer's per-chroot paths are required for fork to work.

With `--network-egress-iface`, every sandbox created with a `network` block gets its own netns, a `/30` from the per-daemon pool (`--network-subnet-pool`, default `10.20.0.0/16`), a veth pair bridged to a tap, a per-netns DHCP server, and a per-sandbox nft chain that only permits egress to IPs the daemon's DNS proxy resolved for allowlisted names — with resolved addresses range-filtered so a guest can't reach link-local/RFC1918/metadata endpoints. See [docs/network.md](docs/network.md) for the networking design.

On startup the daemon reconciles: it re-adopts recorded snapshots and reaps orphaned sandbox state (chroots, netns, nft, processes) left by a previous run, so you don't have to babysit `/srv/jailer` between restarts.

### End-to-end smoke tests

Run as root (jailer + network need `CAP_SYS_ADMIN` + `CAP_NET_ADMIN`):

- [scripts/smoke_fork.sh](scripts/smoke_fork.sh) — fork correctness: boot a source VM, write a marker inside the guest, snapshot, fork ×3, verify each fork sees the marker.
- [scripts/smoke_clone_safety.sh](scripts/smoke_clone_safety.sh) — clone-safety: two forks show distinct `/etc/machine-id`, distinct kernel UUIDs, divergent `/dev/urandom` in a process that straddled the snapshot, per-fork hostname and fork-id.
- [scripts/smoke_e2e.sh](scripts/smoke_e2e.sh) — battery covering exec roundtrip, exit codes, timeouts, OOM kill, structured rusage, default-deny network, allowlist (allowed / denied / IP-literal / `*.domain`), snapshot + multi-fork with per-fork networking, reconcile.
- [scripts/smoke_restart.sh](scripts/smoke_restart.sh) — daemon restart reconciles cleanly (no orphaned VMs/netns/nft).
- [scripts/debug_dns.sh](scripts/debug_dns.sh) — one sandbox, dumps guest- and host-side network state in one shot.

Per-test artifacts land under `/tmp/crucible-smoke-*/` so you can inspect any failing assertion.

### A note on fork cost and your filesystem

Fork restores memory lazily via `userfaultfd`, so guest RAM is **not** copied per fork. The per-fork **rootfs**, however, is cloned: [fsutil.Clone](internal/fsutil/clone.go) prefers `FICLONE` (reflink COW, O(1)) and falls back to a full `io.Copy` when the filesystem lacks reflink support.

**ext4 has no reflink.** Only XFS (`reflink=1`, default since kernel 5.10) and btrfs/f2fs do. If `stat -fc %T <work-base>` returns `ext2/ext3`, each rootfs clone is a full byte-copy. Put `--work-base` on a reflink-capable filesystem for cheap fork.

Latency numbers land here once the bench harness produces reproducible measurements — we'd rather publish none than misleading ones.

### Exercise the API

Full endpoint reference — request/response shapes, the exec frame protocol, and error codes — is in [docs/api.md](docs/api.md).

```bash
# Create a sandbox (body optional; defaults: 1 vCPU, 512 MiB, no timeout).
curl -sS -X POST http://127.0.0.1:7878/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"vcpus": 2, "memory_mib": 512, "timeout_s": 60}'
# → {"id":"sbx_...","vcpus":2,"memory_mib":512,"workdir":"...","created_at":"..."}

# Run a command — response body is a stream of framed stdout/stderr/exit
# records; the last frame carries an ExecResult JSON (exit_code, duration_ms,
# signal, timed_out, oom_killed, and a nested `usage` object).
curl -sS -X POST http://127.0.0.1:7878/sandboxes/sbx_.../exec \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["/bin/uname","-a"]}' --output /tmp/exec.bin

# Snapshot, then fork 5 children from it.
curl -sS -X POST http://127.0.0.1:7878/sandboxes/sbx_.../snapshot   # → {"id":"snp_..."}
curl -sS -X POST 'http://127.0.0.1:7878/snapshots/snp_.../fork?count=5'

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

Each sandbox gets a workdir under `--work-base` (default `/tmp/crucible/run/`) holding the Firecracker API socket, the hybrid-vsock UDS, and `firecracker.log` (guest serial console). `Ctrl-C` / `SIGTERM` gracefully drains active sandboxes.

## Development

```bash
git clone https://github.com/gnana997/crucible && cd crucible
make build && ./crucible version
```

Make targets: `build`, `agent`, `rootfs` (needs `BASE_ROOTFS=`), `profile` (needs `PROFILE=`, docker), `test`, `race`, `vet`, `fmt`, `lint`, `tidy`, `clean`.

Repository layout:

```
cmd/crucible/               CLI entry + daemon wiring
cmd/crucible-agent/         guest-side binary (vsock listener: /exec, /network/refresh, /identity/refresh)
internal/fcapi/             hand-written Firecracker HTTP-over-UDS client
internal/fsutil/            Clone (FICLONE reflink / copy), Move
internal/jailer/            argv builder, chroot staging, cleanup, orphan reap
internal/runner/            firecracker + jailer process lifecycle (Start / Restore)
internal/memfault/          userfaultfd page-fault handler — lazy snapshot memory for fork
internal/sandbox/           ID gen + Manager (lifecycle, exec, snapshot, fork, clone-safety) + durable registry/reconcile
internal/daemon/            HTTP server, routes, middleware, network adapter
internal/agentwire/         shared protocol (frame format, ExecRequest/Result, identity refresh)
internal/agentapi/          host-side client over hybrid-vsock UDS
internal/network/           Manager + subnet pool, per-sandbox netns/veth/tap/bridge, nft base + per-sandbox rules
internal/network/dhcp/      per-netns DHCP responder (SO_BINDTODEVICE-pinned; no MAC filter so forks work)
internal/network/dnsproxy/  DNS proxy (allowlist + resolved-IP range filter + AAAA stripping + rate limiting)
scripts/                    rootfs builder + build-profile + smoke_fork / smoke_clone_safety / smoke_e2e / smoke_restart / debug_dns
profiles/                   profiles.env (profile → base image) + Dockerfile for native language rootfs images
docs/                       VISION.md, ROADMAP.md, architecture.md, api.md, profiles.md, network.md
```

Direct dependencies (kept small on purpose): `golang.org/x/sys` (raw Linux syscalls), `github.com/mdlayher/vsock` (AF_VSOCK listener), `github.com/miekg/dns` (DNS wire format in the proxy), and `github.com/prometheus/client_golang` (the `/metrics` endpoint). Everything else — HTTP, JSON, the Firecracker API client, the hybrid-vsock handshake, the frame protocol, the `userfaultfd` handler — is stdlib + hand-written.

CI runs `go vet`, `gofmt` check, `-race` tests, `go build`, and `golangci-lint` on every push and PR.

## Roadmap (near-term)

- **v0.1** (current): finish the core runtime — a Python SDK, install script + systemd unit.
- **v0.2**: policy files, more language profiles, a custom rootfs builder, hostname-level DNS filtering, a thin Python SDK.

Longer-term direction lives in [docs/ROADMAP.md](docs/ROADMAP.md).

## Security

crucible runs untrusted code, so isolation is a core property — but it is **`v0.1` and not yet hardened for production or untrusted multi-tenant use.** The daemon binds loopback by default and ships without authentication. See [SECURITY.md](SECURITY.md) for the isolation model, current caveats, and how to report a vulnerability.

## Contributing

Early days — the API is stabilizing. If you're building a coding agent and want crucible to fit your workflow, open an issue describing it; concrete use cases shape priorities more than wishlists. Build/test setup, style, and PR guidelines are in [CONTRIBUTING.md](CONTRIBUTING.md); the codebase walk-through is in [docs/architecture.md](docs/architecture.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).

---

*crucible is a working name.*
