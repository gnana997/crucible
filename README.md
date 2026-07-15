# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.6.6](https://img.shields.io/badge/status-v0.6.6-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

![crucible: deploy a serverless postgres on a durable volume; it scales to zero RAM when idle, and the next connection wakes it with its data intact](demo/serverless.gif)

<p align="center"><em>Deploy a <strong>serverless postgres</strong> on a durable volume: it <strong>scales to zero when idle</strong> (freeing its RAM), and the <strong>next connection wakes it</strong> with your data intact (<a href="docs/serverless.md">wake-on-TCP</a> + <a href="docs/volumes.md">persistent volumes</a>). Any TCP service scales to zero the same way. The snapshot/fork primitive underneath is its own one-take demo: <a href="docs/fork.md">snapshot &amp; fork</a>.</em></p>

AI coding agents write code and want to run it: check it compiles, run the tests they just wrote, try three approaches in parallel. Today's options are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, usage-priced), or rolling your own Firecracker stack (months of operational work).

**crucible is the fourth option:** a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives and observability baked in, tuned for running AI-generated code. **Think of it as a safe `docker run` for code you don't trust:** a real guest kernel (a container escape is a VM escape), **default-deny egress**, and one-command **fork** to explore approaches in parallel.

```bash
crucible run nginx:alpine -p 8080:80     # boot an unmodified image, publish a port
crucible cp ./script.py <id>:/work        # drop local code in, no image build, no Dockerfile
crucible shell <id>                        # a real /bin/sh inside it (cd/env persist)
```

Full motivation and design: [docs/VISION.md](docs/VISION.md).

> **Two durability tiers.** A **sandbox** is ephemeral: a daemon restart tears it down (registry records and durable logs persist; the live VM does not), the right contract for "run a sketchy repo, test it, tear it down." An **app** ([docs/apps.md](docs/apps.md)) is durable: the daemon keeps a healthy instance of it, restarts it on failure with backoff, health-checks it, and **re-creates it from spec after a restart or reboot**. `crucible run` for throwaway work; `crucible app create` for a workload you manage over time.

## Highlights

- **Real isolation, not containers.** Every sandbox is a Firecracker microVM under [jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md): its own chroot, mount/PID namespaces, a dropped uid, and cgroup v2 quotas. Not a shared kernel.
- **Snapshot & fork as primitives.** Run setup once, snapshot the warm state, fork *N* parallel children from it. Forks restore with **lazy `userfaultfd` memory**: guest RAM is served from the snapshot on demand, never byte-copied per fork (the AWS Lambda SnapStart technique). See it in one take: [docs/fork.md](docs/fork.md).
- **Durable, self-healing apps you reach (and update) by name.** Promote a workload to a named **app** the daemon keeps alive: restart-on-failure with exponential backoff + a crash-loop guard, http/tcp/exec health checks, config (`--env`), and re-creation from persisted desired state after a daemon restart or host reboot. Reach it through the built-in **ingress proxy** by name (`web.<domain>`) instead of a fixed port; the route follows the app across self-heal and redeploy. `crucible app update` rolls a new instance out **zero-downtime** (boot → ready → flip the route → drain the old one; a failed update keeps the old instance serving), and `app exec`/`logs`/`shell` operate the live instance **by name** ([docs/apps.md](docs/apps.md) · [docs/proxy.md](docs/proxy.md)).
- **Automatic HTTPS on your own domain.** Turn on the ingress proxy's TLS listener with `--acme-email` and it **terminates TLS with a certificate it issues and renews for you** over ACME (Let's Encrypt) — no cert work in the guest. Apps are reachable over HTTPS at their generated `<app>.<domain>` name, and `crucible app domain add web shop.example.com` attaches a **custom domain** that gets its own managed cert too. Issuance is gated to your registered app domains, so a stray hostname can't burn a cert; `:80` redirects to HTTPS. Drop in your own certs for a domain instead, or pick `--tls-mode passthrough` to let the guest own its TLS ([docs/tls.md](docs/tls.md)).
- **Scale to zero.** An app **sleeps when idle and wakes on the next request in under a second**: same IP, same identity, clock stepped to now, and **survives a daemon restart** while asleep. `app create --idle-timeout <dur> --min-scale 0` auto-sleeps an idle app to ~zero RAM through the ingress proxy; the next request wakes it **in place** (a request herd coalesces into one wake) ([docs/apps.md#scale-to-zero](docs/apps.md#scale-to-zero)).
- **Apps talk to each other by name.** Deploy your frontend and API as separate apps; the frontend reaches the API at `http://backend.internal/`, routed through the ingress proxy, **default-deny** (`app create web --can-call backend`), and a scaled-to-zero callee **wakes on the internal call**. Through the proxy VIP, not a guest-to-guest mesh, so per-sandbox isolation stays intact ([docs/apps.md#app-to-app-networking](docs/apps.md#app-to-app-networking)).
- **Scale out: k8s-style, but the VM tradeoffs invert.** `app create --min-scale N` runs N replicas behind the proxy, **P2C-balanced**; `--max-scale M` autoscales on request concurrency (fast up, slow down). Each replica is **forked warm from a snapshot in milliseconds**, not cold-booted, so scale-up is cheap where containers repay a cold start every time ([docs/apps.md#horizontal-scale-out](docs/apps.md#horizontal-scale-out)).
- **Persistent volumes.** Attach a durable, fsync-honest block device to any sandbox or app (`--volume NAME:/path`): data survives destroy/re-create, a hard VM kill, an app redeploy, sleep, and a daemon restart. A volume-backed app (postgres, sqlite) is single-writer (it redeploys destroy-then-boot and sleeps stop/start) so **stateless stays magic, stateful gets durable** ([docs/volumes.md](docs/volumes.md)).
- **Serverless for any TCP service (wake-on-TCP).** Scale-to-zero for anything that speaks TCP (postgres, mysql, redis, mongo, your own daemon), not just HTTP. A scale-to-zero app that publishes a port (`-p 5432:5432 --min-scale 0 --idle-timeout 30s`) is fronted by an L4 forwarder that **wakes it on the first connection** and sleeps it when idle, with no proxy in the path, protocol-agnostic. Idle pooled connections are reaped so it truly reaches zero; `--keep-connections` flips it to **connection-scoped** mode for pub/sub (awake while subscribed, asleep when nobody's connected). On a volume, that's a **self-hosted serverless postgres or redis**: zero RAM until someone connects, then **wakes in about 170 ms** on the next `psql` from a snapshot (no cold boot, no WAL recovery), data intact ([docs/serverless.md](docs/serverless.md)).
- **Private registries.** Pull authenticated images: `crucible registry login <host>` stores a per-registry credential on the daemon, so `run`, `app create`, and an app's re-pull on restart can fetch private images (Docker Hub, GHCR, GitLab, Quay, self-hosted, static GCP/ACR). Credentials live on the daemon (a durable app on a private image survives a reboot) and are never read from your local `~/.docker/config.json` ([docs/registry.md](docs/registry.md)).
- **Clone-safety.** Each fork wakes with a fresh kernel RNG seed, a rotated `machine-id`, and its own hostname, ordered *before* the fork is reachable, so no two forks silently share UUIDs, secrets, or entropy.
- **Default-deny networking, opt-in wider.** No egress unless you allowlist hostnames; resolved IPs are range-filtered so a guest can't SSRF cloud metadata or private ranges. A trusted app can opt into full public egress (`--net-full-egress`) or public CIDRs (`--net-allow-cidr`), still public-unicast only, never metadata/RFC1918. Enforced in host nftables + a DNS proxy the guest is forced through.
- **Three ways to drive it.** A [CLI](docs/cli.md), a live [TUI dashboard](docs/tui.md), and an [MCP server](docs/mcp.md): all thin clients over one REST API, so they can't drift.
- **Scoped tokens.** Bind an API key to a policy the daemon enforces (allowed operations, egress ceiling, profile allowlist, resource caps, expiry). See [docs/policy.md](docs/policy.md).
- **Observability.** Per-exec structured results (exit code, wall-clock, CPU/memory/I/O usage) and durable per-sandbox logs, plus **per-app metrics** on Prometheus `/metrics` (RPS, latency, replicas, `app_asleep`) with a reference Grafana dashboard, **OTLP export** of metrics + logs to any collector (`--otlp-endpoint`), daemon **pprof**, and on-demand **packet capture** (`app capture` → host-side pcap) ([docs/observability.md](docs/observability.md)).
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
| Fork (warm → child), p50 | ~530 ms | **~125 ms** |
| Fork throughput (64-way) | 3.4/s | **41/s** |
| **Proxy wake (request → served), p50** | 252 ms | **125 ms** |
| 64 forks, host RAM | 1.0 GiB | **813 MiB** (~13 MiB/fork, pages shared; vs 32 GiB naïve) |
| Exec roundtrip | ~2 ms | ~3 ms |

**Fork is ~2.6× faster than a cold create on ext4, ~8× on reflink**: pay the ~1 s boot once, then branch cheaply. A slept app **wakes in ~125 ms** (~8× faster than a cold create, and barely storage-dependent), and we ran **320 concurrent microVMs** on the laptop with RAM to spare.

> **By the numbers:** one static binary · no guest RAM copied per fork · 3 interfaces (CLI · TUI · MCP) · 35 MCP tools · 8 prebuilt profiles · 512 MB / 1 vCPU / 60 s safe defaults

## Roadmap

crucible is at **v0.6.6**. One highlight per release line below, the full shipped-vs-planned history and capability matrix live in **[docs/ROADMAP.md](docs/ROADMAP.md)**.

- **v0.7.0: real HTTPS deploys.** Headline: **TLS termination at the ingress proxy with automatic certificates**. Open the proxy's TLS listener and set `--acme-email`, and the proxy terminates TLS with a cert it issues and renews over ACME (Let's Encrypt) — on the generated `<app>.<domain>` name and on **custom domains** you attach (`crucible app domain add`), each globally unique. Issuance is gated to your registered app domains so a stray SNI can't burn a cert; `:80` serves the HTTP-01 challenge and redirects to HTTPS; HTTP-01 and TLS-ALPN-01 are both answered; renewal is automatic. Bring your own certs (`<cert-dir>/manual/`) or keep an app on SNI passthrough (`--tls-mode passthrough`) instead ([docs/tls.md](docs/tls.md)).
- **v0.6.x: persistent volumes & serverless-any-TCP.** Headline: **durable data that outlives the sandbox**, attaching an fsync-honest block device to any sandbox or app (`--volume NAME:/path`), surviving destroy/re-create, a hard VM kill, an app redeploy, sleep, and a daemon restart. v0.6.1 adds **wake-on-TCP**: a scale-to-zero app's published port wakes it on the first connection, protocol-agnostic, so any volume-backed database (postgres, redis, …) becomes a *self-hosted serverless* service that costs zero RAM until someone connects, plus a `--keep-connections` mode for scale-to-zero pub/sub. v0.6.2 makes that wake **instant (~170 ms snapshot restore, no cold boot or WAL recovery)** for stateful apps. v0.6.3 adds **volume backups**: a point-in-time `volume backup` / `restore` / `clone`, consistency-aware (a live database is frozen with `fsfreeze` for the instant of the copy, so it is backed up with no downtime), kept on your own storage via `--backup-dir`. v0.6.4 is **operate with confidence**: **upgrade the daemon without dropping apps** (`app sleep --all` drains the fleet, the restart re-adopts every app, and they wake on demand — rehearsed against the previous release so cross-version snapshot-wake is measured), a one-command **daemon backup** (`crucible admin backup`), disk-usage metrics for scale-to-zero density, and **IPv6 at the edge**. v0.6.5 adds **capacity guards**: a `--sleep-min-free-disk-mib` floor so a snapshotting fleet can't fill the disk (the complement to the RAM wake floor), and a mass-wake load test proving 20 apps wake concurrently in ~430 ms p99 with RAM barely moving (lazy paging faults in only each guest's working set). v0.6.6 adds **off-host backups**: `volume backup export`/`import` stream a backup's bytes off the host and back (gzip by default) so a control plane can ship them to an object store, while the daemon stays provider-agnostic — no cloud SDKs or credentials in it ([docs/upgrades.md](docs/upgrades.md), [docs/apps.md](docs/apps.md), [docs/backups.md](docs/backups.md), [docs/serverless.md](docs/serverless.md)).
- **v0.5.x: apps as a platform.** Headline: **scale to zero**: an app *sleeps when idle and wakes on the next request in under a second* (same IP + identity, clock stepped to now), surviving a daemon restart while asleep ([docs/apps.md#scale-to-zero](docs/apps.md#scale-to-zero)). The line also adds **app→app networking**, **horizontal scale-out** (replicas warm-forked from a snapshot, autoscaled), and **observability** (per-app metrics + OTLP export + host-side packet capture).
- **v0.4.x: durable apps you deploy.** Headline: **durable, self-healing apps**: `crucible app create` promotes a workload to a named app the daemon keeps alive (restart + backoff, health checks) and re-creates from spec after a restart or reboot ([docs/apps.md](docs/apps.md)). Plus reach-by-name (the **ingress proxy**), **zero-downtime `app update`**, and **private registries**.
- **v0.3.x: the safe `docker run`.** Boot unmodified OCI images (`run` / `build`), drop code in with `crucible cp`, an interactive `crucible shell` + TUI, and the public Go SDK.
- **Next (planned):** usage metering (per-app request/compute accounting), incremental volume backups, and wildcard / DNS-01 certificates.

## Security

crucible runs untrusted code, so isolation is a core property, but it is **pre-1.0 and not yet hardened for production or untrusted multi-tenant use.** The daemon binds loopback by default and ships with optional bearer-key auth (required, with TLS, to bind a non-loopback address). See [SECURITY.md](SECURITY.md) for the isolation model, current caveats, and how to report a vulnerability.

## Contributing

Early days: the API is stabilizing. If you're building a coding agent and want crucible to fit your workflow, open an issue describing it; concrete use cases shape priorities more than wishlists. Build/test setup, style, and PR guidelines are in [CONTRIBUTING.md](CONTRIBUTING.md); the codebase walk-through is in [docs/architecture.md](docs/architecture.md). By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
