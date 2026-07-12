# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.5.3](https://img.shields.io/badge/status-v0.5.3-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

![crucible: a durable app sleeps to ~zero RAM when idle, survives a full daemon restart while asleep, then wakes in place on the next request in under a second](demo/scale-to-zero.gif)

<p align="center"><em>Deploy a durable app reached by name; it <strong>sleeps itself when idle</strong> (freeing its RAM), survives a <strong>full daemon restart</strong> while asleep, then <strong>wakes in place on the next request in under a second</strong> — same address, same identity (<a href="docs/apps.md#scale-to-zero">scale to zero</a>). The snapshot/fork primitive underneath is its own one-take demo: <a href="docs/fork.md">snapshot &amp; fork</a>.</em></p>

AI coding agents write code and want to run it: check it compiles, run the tests they just wrote, try three approaches in parallel. Today's options are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, usage-priced), or rolling your own Firecracker stack (months of operational work).

**crucible is the fourth option:** a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives and observability baked in, tuned for running AI-generated code. **Think of it as a safe `docker run` for code you don't trust:** a real guest kernel (a container escape is a VM escape), **default-deny egress**, and one-command **fork** to explore approaches in parallel.

```bash
crucible run nginx:alpine -p 8080:80     # boot an unmodified image, publish a port
crucible cp ./script.py <id>:/work        # drop local code in, no image build, no Dockerfile
crucible shell <id>                        # a real /bin/sh inside it (cd/env persist)
```

Full motivation and design: [docs/VISION.md](docs/VISION.md).

> **Two durability tiers.** A **sandbox** is ephemeral: a daemon restart tears it down (registry records and durable logs persist; the live VM does not) — the right contract for "run a sketchy repo, test it, tear it down." An **app** ([docs/apps.md](docs/apps.md)) is durable: the daemon keeps a healthy instance of it, restarts it on failure with backoff, health-checks it, and **re-creates it from spec after a restart or reboot**. `crucible run` for throwaway work; `crucible app create` for a workload you manage over time.

## Highlights

- **Real isolation, not containers.** Every sandbox is a Firecracker microVM under [jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md): its own chroot, mount/PID namespaces, a dropped uid, and cgroup v2 quotas. Not a shared kernel.
- **Snapshot & fork as primitives.** Run setup once, snapshot the warm state, fork *N* parallel children from it. Forks restore with **lazy `userfaultfd` memory**: guest RAM is served from the snapshot on demand, never byte-copied per fork (the AWS Lambda SnapStart technique). See it in one take: [docs/fork.md](docs/fork.md).
- **Durable, self-healing apps you reach — and update — by name.** Promote a workload to a named **app** the daemon keeps alive: restart-on-failure with exponential backoff + a crash-loop guard, http/tcp/exec health checks, config (`--env`), and re-creation from persisted desired state after a daemon restart or host reboot. Reach it through the built-in **ingress proxy** by name (`web.<domain>`) instead of a fixed port; the route follows the app across self-heal and redeploy. `crucible app update` rolls a new instance out **zero-downtime** (boot → ready → flip the route → drain the old one; a failed update keeps the old instance serving), and `app exec`/`logs`/`shell` operate the live instance **by name** ([docs/apps.md](docs/apps.md) · [docs/proxy.md](docs/proxy.md)).
- **Scale to zero.** An app **sleeps when idle and wakes on the next request in under a second** — same IP, same identity, clock stepped to now — and **survives a daemon restart** while asleep. `app create --idle-timeout <dur> --min-scale 0` auto-sleeps an idle app to ~zero RAM through the ingress proxy; the next request wakes it **in place** (a request herd coalesces into one wake) ([docs/apps.md#scale-to-zero](docs/apps.md#scale-to-zero)).
- **Apps talk to each other by name.** Deploy your frontend and API as separate apps; the frontend reaches the API at `http://backend.internal/`, routed through the ingress proxy — **default-deny** (`app create web --can-call backend`), and a scaled-to-zero callee **wakes on the internal call**. Through the proxy VIP, not a guest-to-guest mesh, so per-sandbox isolation stays intact ([docs/apps.md#app-to-app-networking](docs/apps.md#app-to-app-networking)).
- **Scale out — k8s-style, but the VM tradeoffs invert.** `app create --min-scale N` runs N replicas behind the proxy, **P2C-balanced**; `--max-scale M` autoscales on request concurrency (fast up, slow down). Each replica is **forked warm from a snapshot in milliseconds** — not cold-booted — so scale-up is cheap where containers repay a cold start every time ([docs/apps.md#horizontal-scale-out](docs/apps.md#horizontal-scale-out)).
- **Private registries.** Pull authenticated images: `crucible registry login <host>` stores a per-registry credential on the daemon, so `run`, `app create`, and an app's re-pull on restart can fetch private images (Docker Hub, GHCR, GitLab, Quay, self-hosted, static GCP/ACR). Credentials live on the daemon — a durable app on a private image survives a reboot — and are never read from your local `~/.docker/config.json` ([docs/registry.md](docs/registry.md)).
- **Clone-safety.** Each fork wakes with a fresh kernel RNG seed, a rotated `machine-id`, and its own hostname, ordered *before* the fork is reachable, so no two forks silently share UUIDs, secrets, or entropy.
- **Default-deny networking, opt-in wider.** No egress unless you allowlist hostnames; resolved IPs are range-filtered so a guest can't SSRF cloud metadata or private ranges. A trusted app can opt into full public egress (`--net-full-egress`) or public CIDRs (`--net-allow-cidr`) — still public-unicast only, never metadata/RFC1918. Enforced in host nftables + a DNS proxy the guest is forced through.
- **Three ways to drive it.** A [CLI](docs/cli.md), a live [TUI dashboard](docs/tui.md), and an [MCP server](docs/mcp.md): all thin clients over one REST API, so they can't drift.
- **Scoped tokens.** Bind an API key to a policy the daemon enforces (allowed operations, egress ceiling, profile allowlist, resource caps, expiry). See [docs/policy.md](docs/policy.md).
- **Observability.** Per-exec structured results (exit code, wall-clock, and CPU/memory/I/O usage), durable per-sandbox logs, and a Prometheus `/metrics` endpoint.
- **Self-hosted, single binary.** Daemon and CLI are one Go binary. No cloud, no account, no telemetry.

> **Maturity.** crucible is pre-1.0 and **not yet hardened for production or untrusted multi-tenant use.** The daemon binds loopback by default, with optional bearer-key auth (required, plus TLS, to bind a non-loopback address). See [SECURITY.md](SECURITY.md) for the exact isolation model and its limits.

## Quick start

crucible is a **Linux daemon** (it needs KVM + Firecracker) plus a **cross-platform client**: the CLI, TUI, and `crucible mcp serve` are thin HTTP clients, so they run on macOS and Windows too, driving a remote Linux daemon.

**Linux (a host where `ls /dev/kvm` works)**: one command fetches and checksum-verifies firecracker + jailer, a guest kernel, and a rootfs, then starts the service:

```bash
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo bash -s -- --with-deps --enable
```

Then run your first sandbox:

```bash
crucible run nginx:alpine -p 8080:80 && curl localhost:8080   # boots + serves
crucible tui                                                  # live dashboard
```

**macOS / Windows**: install just the client (no root, no VM) and point it at a Linux daemon:

```bash
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sh -s -- --client --addr https://YOUR-LINUX-HOST:7878 --token <key>
```

The daemon installer's `--connect-token` mints a scoped key and prints that exact client line plus an MCP snippet to paste into Claude Code / Cursor. Manual setup and the platform matrix are in [`install.sh --help`](install.sh) and [docs/cli.md](docs/cli.md); building from source is in [CONTRIBUTING.md](CONTRIBUTING.md).

## Usage

Daemon-authoritative: the CLI, TUI, and MCP server are thin clients over one REST API; point them with `--addr` (or `CRUCIBLE_ADDR`; default `127.0.0.1:7878`) and `--token`.

```bash
crucible run nginx:alpine -p 8080:80              # boot an OCI image, publish a port (long-lived)
crucible build -t myapp . && crucible run myapp   # a repo's Dockerfile → running, in two lines
crucible cp ./app.py <id>:/work                   # drop local code in, no image build
crucible shell <id>                               # interactive shell inside it (no PTY)
SBX=$(crucible run --profile python-3.12)
crucible snapshot create $SBX | xargs crucible fork --count 5   # explore 5 approaches in parallel
```

- **CLI**: the full reference (all commands, flags, exit codes) is in [docs/cli.md](docs/cli.md). Add `-o json` to any command for machine-readable output.
- **TUI**: `crucible tui` opens a live dashboard: running sandboxes, the fork tree, streaming `exec`, and a k9s-style logs view, all gated on the token's scope. [docs/tui.md](docs/tui.md).
- **MCP**: `crucible mcp serve` exposes crucible to any [MCP](https://modelcontextprotocol.io) agent (Claude Code, Cursor, …) as native tools (create, run, exec, snapshot, fork, `write_files`, `read_file`, and more) with operator guardrails. [docs/mcp.md](docs/mcp.md).
- **REST**: everything above is the daemon's HTTP API: endpoints, the exec frame protocol, and error codes are in [docs/api.md](docs/api.md).

## How it works

A single Go binary is both the daemon and the CLI; each guest runs a small vsock agent. The daemon boots Firecracker microVMs under jailer, runs commands over vsock with streamed output, captures snapshots, and forks end-to-end, each fork restored with lazy `userfaultfd` memory, its own netns + DHCP-assigned IP behind a default-deny allowlist, and a per-fork identity refresh. A durable registry is journaled and reconciled on restart (orphaned VMs / netns / nft rules are reaped). Full walkthrough: [docs/architecture.md](docs/architecture.md); networking has its own [design doc](docs/network.md).

## Performance

Measured on one 24-core box, 512 MiB sandboxes. The `--work-base` filesystem is the biggest lever: reflink (btrfs/XFS) makes fork's rootfs clone O(1), while **ext4 has no reflink, so it byte-copies**, so here's both. Full methodology in [docs/benchmarks.md](docs/benchmarks.md):

| | ext4 (common default) | btrfs / XFS (reflink) |
|---|---|---|
| Fork (warm → child) | ~690 ms | **~207 ms** |
| Fork throughput (64-way) | 3.7/s | **45/s** |
| 128 forks, host RAM | 4.9 GB | **1.2 GB** (vs 64 GB naïve copy) |
| Exec roundtrip | ~3 ms | ~3 ms |

Fork is **~9× faster than a cold boot** either way, and we ran **512 concurrent microVMs** on the laptop (reflink, RAM-bound).

> **By the numbers:** one static binary · no guest RAM copied per fork · 3 interfaces (CLI · TUI · MCP) · 24 MCP tools · 8 prebuilt profiles · 512 MB / 1 vCPU / 60 s safe defaults

## Roadmap

- **v0.5.3** (current): **reliability & isolation hardening** — no orphaned VMs across app-lifecycle edges (a rolling update's old instance is always reaped, even on delete/re-update/sleep mid-drain); a daemon upgrade no longer boots a **stale guest agent** from a cached image (conversions are keyed by the injected agent too); and a published host port now **coexists** with the `<app>.internal` VIP on the same port (`SO_REUSEPORT`). ([CHANGELOG](CHANGELOG.md#053--2026-07-13)).
- **v0.5.2**: **scale out** — `app create --min-scale N` runs N replicas behind the proxy, **P2C load-balanced**; `--max-scale M --target-concurrency C` autoscales on concurrency (fast up, slow down). Each replica is **forked warm from a golden snapshot in milliseconds**, self-healed by the reconciler — k8s-style horizontal scaling where the VM properties (snapshot/restore) make scale-up cheap ([docs/apps.md#horizontal-scale-out](docs/apps.md#horizontal-scale-out)).
- **v0.5.1**: **app→app networking** — deploy web + backend as separate apps and let them talk: `web` reaches `http://backend.internal/` through the ingress proxy, **default-deny** (`app create web --can-call backend`), and a scaled-to-zero backend **wakes on the internal call**. Through the proxy VIP (not a guest-to-guest mesh), so per-sandbox isolation holds. Experimental, off by default (`--internal-networking`) ([docs/apps.md](docs/apps.md)).
- **v0.5.0**: **scale to zero** — an app **sleeps when idle and wakes on the next request in under a second**. `crucible app sleep`/`app wake` snapshot a running app and stop its VMM to free RAM+CPU, then restore it **in place** (same IP, same identity, clock stepped to now); `app create --idle-timeout <dur> --min-scale 0` does it automatically — the ingress proxy sleeps an idle app and the next request wakes it, buffered until it's ready (a request herd coalesces into one wake). A slept app **survives a daemon restart** (durable snapshot, re-adopted on start) ([docs/apps.md](docs/apps.md)).
- **v0.4.4**: **private registries** — `crucible registry login <host>` stores a per-registry credential on the daemon so `run`, `app create`, and an app's re-pull on restart can fetch private images (Docker Hub, GHCR, GitLab, Quay, self-hosted, static GCP/ACR); plus a one-shot `run --registry-auth` for CI. Never reads your `~/.docker/config.json` ([docs/registry.md](docs/registry.md)).
- **v0.4.3**: **operate & safe-update** — `crucible app update` rolls a new instance out **zero-downtime** (boot → readiness gate → flip the ingress route → drain the old one; a failed update keeps the old instance serving), and you drive a running app **by name** — `app exec`/`logs`/`shell` plus MCP `app_exec`/`app_logs` — resolved to the live instance per call, so it survives a self-heal or redeploy ([docs/apps.md](docs/apps.md)).
- **v0.4.2**: **reach an app by name** — a daemon-owned ingress proxy routes inbound traffic to an app's *current* instance by name (`web.<domain>`, Host-header L7 or SNI passthrough L4), the route following the app across self-heal and redeploy; plus in-place `crucible app update` and health seeded from an image's Docker `HEALTHCHECK` ([docs/proxy.md](docs/proxy.md)).
- **v0.4.1**: **apps you can actually deploy** — `-e/--env` config, exec (`--health-cmd`) health checks, `-P` publish-all from the image's `EXPOSE`, and real egress for trusted workloads (`--net-full-egress` + `--net-allow-cidr`, public-hosts-only).
- **v0.4.0**: **durable, self-healing apps** — `crucible app create` promotes a workload to a named app the daemon keeps alive (restart + backoff + crash-loop guard, http/tcp health) and **re-creates from spec after a daemon restart or reboot** ([docs/apps.md](docs/apps.md)); plus fork with `-p` port publish. Sandboxes stay the ephemeral primitive.
- **v0.3.x**: the safe `docker run` for untrusted/AI code — OCI image boot + `crucible build`, `crucible cp` + MCP `write_files`/`read_file`, an interactive `crucible shell`, a TUI logs view, `--disk` sizing, top-level `stop`/`rm`, durable logs, and the public Go SDK.
- **Next (planned):** TLS termination at the proxy, native cloud-registry auth, a PTY for full-terminal sessions, volumes for stateful apps, and an observability/OpenTelemetry pipeline (v0.5.x).

Full shipped-vs-planned capability matrix: [docs/ROADMAP.md](docs/ROADMAP.md).

## Security

crucible runs untrusted code, so isolation is a core property, but it is **pre-1.0 and not yet hardened for production or untrusted multi-tenant use.** The daemon binds loopback by default and ships with optional bearer-key auth (required, with TLS, to bind a non-loopback address). See [SECURITY.md](SECURITY.md) for the isolation model, current caveats, and how to report a vulnerability.

## Contributing

Early days: the API is stabilizing. If you're building a coding agent and want crucible to fit your workflow, open an issue describing it; concrete use cases shape priorities more than wishlists. Build/test setup, style, and PR guidelines are in [CONTRIBUTING.md](CONTRIBUTING.md); the codebase walk-through is in [docs/architecture.md](docs/architecture.md). By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
