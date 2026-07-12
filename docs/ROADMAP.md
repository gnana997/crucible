---
title: Roadmap
description: "What has shipped and what is planned, in rough order, grouped into coherent milestones."
---

# Roadmap

What's shipped and what's planned, roughly in order. Each section is a coherent milestone — items inside a section are the ones that landed (or are expected to land) together. A `[x]` is actually working on `main`; a `•` is planned. Nothing is called "done" until it ships.

For the motivation and design principles behind these choices, see [VISION.md](VISION.md).

---

## Shipped

### v0.1 — Core runtime

The minimal usable thing: boot a sandbox, run a command inside it, get a structured result back, and fork it cheaply.

- [x] Firecracker orchestration in Go (boot a microVM via the Firecracker API), under the jailer (chroot + mount/PID namespaces + privilege drop)
- [x] HTTP API: create / list / inspect / delete sandboxes, exec, snapshot, fork
- [x] Snapshot + fork primitives — a fork serves guest memory lazily from the snapshot via `userfaultfd` (no per-fork RAM copy)
- [x] Clone-safety — per-fork kernel RNG reseed and machine-identifier rotation, applied before a fork is reachable
- [x] Default-deny networking — each sandbox in its own netns, egress limited to a DNS-proxy hostname allowlist with range-filtered resolved addresses
- [x] Per-request resource ceilings (vCPU, memory, fork fan-out) plus a per-sandbox lifetime and a per-exec deadline
- [x] Structured execution record per exec (exit code, timing, signal, timeout/OOM flags, and CPU/memory/page-fault/context-switch/IO usage)
- [x] Durable sandbox registry with reconcile-on-restart, and structured JSON lifecycle logs
- [x] Host-side cgroup quotas (cpu.max / memory.max / pids.max) sized per sandbox, on by default under jailer
- [x] Prometheus `/metrics` endpoint — `sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds`
- [x] Native language rootfs profiles (`base`, `python`, `node`, `go`, versioned) — built from official language images via `make profile` ([profiles.md](profiles.md))
- [x] CLI over the REST API — `sandbox`, `snapshot`, `fork`, `profile ls`, and a `run` one-shot, on a reusable typed Go client; `-o json` everywhere ([cli.md](cli.md))
- [x] Install script + systemd unit — `install.sh` drops the binary, a `crucible.service` unit, and a config template

### v0.1.2 — MCP server + API-key auth

- [x] **MCP server** (`crucible mcp serve`) — a stdio [Model Context Protocol](https://modelcontextprotocol.io) server so any MCP agent (Claude Code, Cursor, …) drives crucible as native tools, with operator guardrails the agent can't widen. Built on the `sdk` Go package, so an MCP call and the CLI hit the identical path and can't drift ([mcp.md](mcp.md)).
- [x] **Daemon API-key auth** — bearer keys hashed at rest; once any key exists every request must present it, and binding a non-loopback address is refused without keys **and** TLS ([SECURITY.md](../SECURITY.md)).

### v0.1.3 — Scoped / policy tokens

- [x] Bind a key to a policy the **daemon** enforces — allowed operations, an egress ceiling, a profile allowlist, resource caps, and an expiry — so a handed-out key is worthless beyond its bounds. `crucible policy validate/show`, `GET /whoami` ([policy.md](policy.md)).

### v0.2.0 — TUI + fork lineage

- [x] **TUI** (`crucible tui`) — a live terminal dashboard: running sandboxes, the fork tree, and interactive streaming `exec`, with create/snapshot/fork/delete gated on the token's scope. A thin consumer of the `sdk` Go package ([tui.md](tui.md)).
- [x] **Fork lineage on the API** — `source_snapshot_id` records which snapshot a sandbox was forked from, so the fork genealogy is reconstructable by any client (this is what the tree view draws).

### v0.3.x — The safe `docker run` for untrusted/AI code

- [x] **OCI image boot** — `crucible run <image>` boots an unmodified image's entrypoint in a microVM; `crucible build` builds a Dockerfile and loads it into the store (daemon stays Docker-free). Publish host ports with `-p`.
- [x] **Interactive shell** — `crucible shell <id>` / `sandbox exec -i`: a real long-lived `/bin/sh` over a hijacked full-duplex vsock stream (state persists; line-buffered, **no PTY**). The TUI gains a **scrollback + `tab`-to-shell** session view.
- [x] **`--disk`** per-sandbox writable sizing (`resize2fs` the clone, never the shared image); top-level **`stop`/`rm`** ops verbs; durable **`logs`**.
- [x] **MCP for the wedge** — `image`/`pull`/`publish`/`disk_mib` on `create_sandbox`/`run`, plus `logs` and `stop_sandbox` tools ([mcp.md](mcp.md)).
- [x] **Complete orphan reaping** — startup sweeps live orphan processes and empty orphan cgroups; a killed daemon leaves no lingering firecracker.
- [x] **Copy files in / out** — `crucible cp <local> <sbx>:<path>` (and back out), tar over vsock — the safe-*copy* model (not a host bind-mount), tar-slip-safe and size-bounded on the way out; plus MCP `write_files`/`read_file`. Drop code in and run it, no image build.

### v0.4.0 — Durable, self-healing apps

- [x] **Durable app model** — `crucible app create <name> --image …` promotes a workload to a named **app** the daemon keeps a healthy instance of. Desired state lives in a bbolt control-plane store; the ephemeral `sandbox` primitive is unchanged ([apps.md](apps.md)).
- [x] **Survives restart** — the app reconciler re-creates each app's instance from spec after a daemon restart or host reboot (desired-state reconcile, the Fly/k8s model — *re-created*, not live-re-attached; in-VM memory is lost, cost is one cold boot).
- [x] **Self-heal** — daemon-side restart-on-failure with **exponential backoff + a crash-loop guard**, plus **http/tcp health checks** (declarative `always`/`on-failure`/`never` policy).
- [x] **Full surface** — `crucible app ls|get|rm|logs|exec|shell`, REST `/apps`, the Go SDK (`CreateApp`/`ListApps`/`GetApp`/`DeleteApp` + an `App` handle), and four MCP tools (`create_app`/`list_apps`/`get_app`/`delete_app`, → 19 tools).
- [x] **`crucible fork -p HOST:GUEST`** — publish a host port on a fork (a running server, forked and exposed on its own port).

- **Durability contract:** an **app** survives a daemon restart (re-created from desired state); a bare **sandbox** does not (it stays the throwaway primitive). Live-VM re-attach (avoiding the cold boot) is later trajectory work.

### v0.4.1 — Apps you can actually deploy

Turn a durable app from *survivable* into *deployable*: real config and real egress, across `app create`, `run`, and `sandbox create`.

- [x] **App env** — `-e/--env KEY=VALUE` delivered to the entrypoint (image `ENV` < your `--env`).
- [x] **Real egress** — `--net-full-egress` (reach any public host) and `--net-allow-cidr 203.0.113.0/24` (public IP literals) for a workload you deploy yourself. **Public-hosts-only, no exceptions** — metadata/link-local/RFC1918/CGNAT/reserved are always dropped (the nft guard is unit-tested to agree with the DNS-layer SSRF filter), gated by a `net_full_egress` scoped-token grant.
- [x] **Exec health checks** — `--health-cmd '<command>'` runs a command in the guest (exit 0 = healthy), joining http/tcp.
- [x] **Publish declared ports** — `-P/--publish-all` publishes every port the image `EXPOSE`s (guest N → host N).

### v0.4.2 — Reach it by name

The durable app is now reachable by name and updatable in place.

- [x] **Ingress proxy** — reach an app by name instead of a published port. `--proxy-listen` (Host-header routing, L7), `--proxy-tls-listen` (SNI passthrough, L4 — the guest terminates its own TLS), `--proxy-domain <domain>` (`web.<domain>` → app `web`). Off in the daemon by default; the installer enables it on `:7879` (next to the API, clear of the `:80`/`:8080`/… ports you publish to), overridable to `:80`/`:443` for a production ingress. Resolution is live, so the route follows the app across self-heal and redeploy and never points at a stale IP ([proxy.md](proxy.md)).
- [x] **`crucible app update`** — replace an app's spec (name immutable) and redeploy: the reconciler bumps the generation, destroys the old instance, and boots a fresh one. Also on the Go SDK (`UpdateApp`) and the MCP `update_app` tool. *(v0.4.3 makes this zero-downtime — see below.)*
- [x] **Health seeded from the image** — an app that declares no health inherits the image's Docker `HEALTHCHECK` (as an `exec` check) when present.
- [x] **Inbound isolation** — inbound reaches a guest only from the daemon over its veth; peers can't reach each other and a guest can't reach the proxy listeners at all, so the proxy is not a lateral path.

### v0.4.3 — Operate & safe-update

Update a deployed app without dropping traffic, and drive a running app by name.

- [x] **Zero-downtime rolling `app update`** — for a proxy-fronted app the reconciler boots the new instance, waits for a **readiness gate** (its health check, or a TCP connect to the app's port), **flips the ingress route** to it, then drains the old instance before destroying it. The proxy follows the flip, so the cutover drops nothing. A **failed** update aborts and keeps the old instance serving (never takes the app down); `status.instance_generation` shows which spec is live.
- [x] **Operate an app by name** — `crucible app exec`/`logs`/`shell` (and MCP `app_exec`/`app_logs`) resolve the app's **current** instance server-side per call, so they survive a self-heal or redeploy. `app logs -f` reattaches to the new instance across a roll. Flag parity added (`app exec --cwd/--timeout/-e`, `app shell --shell`).

### v0.4.4 — Private registries

Pull authenticated images, with the credential on the daemon.

- [x] **Private-registry pull** — `crucible registry login/logout/ls` stores a per-registry credential on the daemon (`--registry-store`, `0600`, gated by a new `registry` scoped-token op) that feeds every pull: `run`, `app create`, and an app's **re-pull on restart** (so a durable app on a private image survives a reboot). Static creds cover Docker Hub, GHCR, GitLab, Quay, self-hosted, and static GCP/ACR; ECR via `aws ecr get-login-password`. Never reads `~/.docker/config.json`. Plus one-shot `run --registry-auth` for CI ([registry.md](registry.md)).

### v0.5.0 — Scale to zero

An app sleeps when idle and wakes on the next request in under a second — same IP, same identity, correct clock — and survives a daemon restart while asleep. Built by re-pointing machinery crucible already ships (snapshot/restore with lazy memory, clone-safety, the reconciler, the ingress proxy) at a new policy, not a new subsystem.

- [x] **App sleep/wake** — `crucible app sleep`/`app wake` snapshot a running app and stop its VMM to free RAM + CPU while keeping the netns, subnet/IP reservation, and ingress route, then restore it **in place**: same instance id, same IP (no DHCP bounce), CRNG reseeded and clock stepped to host time before it's reachable, but — unlike a fork — machine-id/hostname *not* rotated. On the Go SDK (`SleepApp`/`WakeApp`) and MCP (`app_sleep`/`app_wake`) ([apps.md](apps.md)).
- [x] **Automatic scale-to-zero** — `app create --idle-timeout <dur> --min-scale 0` drops an idle app to ~zero RAM: the ingress proxy tracks per-app last-activity + open connections and, once idle and healthy, the reconciler sleeps it. The next request **triggers a wake, holds the request, and forwards it once the app passes readiness** — a request herd coalesces into a single wake. `--min-scale ≥1` stays always-warm; `--idle-timeout 0` never sleeps.
- [x] **Survives a daemon restart while asleep** — sleep captures a durable snapshot (journaled record + cloned rootfs), so a slept app is re-adopted on daemon start and the first post-restart request wakes a fresh instance from it.
- [x] **Wake admission gate** — a wake is refused (`503`, app stays asleep) when host free memory is below `--wake-min-free-mib` (default 256), plus `snapshots_active` and an `app_wake_latency_seconds` histogram on `/metrics`.

### v0.5.1 — App→app networking *(current)*

Deploy your frontend and API as separate apps and let them talk — the first step from "run an app" to "deploy a system." Stateless service-to-service, so it doesn't wait on volumes.

- [x] **Reach another app by name** — with the daemon's `--internal-networking`, an app calls another at `http://<app>.internal/`, routed **through the ingress proxy VIP** (the DNS anycast) to the callee's current instance. Because it goes through the proxy — not a direct guest-to-guest path — an internal call inherits **wake-on-request** (a scaled-to-zero callee wakes and serves) and leaves per-sandbox isolation intact (a guest still can't reach a peer's IP directly) ([apps.md](apps.md)).
- [x] **Default-deny authorization** — `app create <app> --can-call <other>` declares the calls an app may make (empty = none). Enforced daemon-side: the proxy returns 403 on an un-granted call, and DNS answers `<app>.internal` only for granted callers (else NXDOMAIN — no discovery of apps you can't call). On the Go SDK (`AppSpec.CanCall`) and MCP; `app_internal_requests_total` on `/metrics`. Experimental, off by default.

## Planned

### Next — Production images & deploys

The app model, its front door, zero-downtime updates, operate-by-name, private-registry pull, scale-to-zero, and app→app networking exist (v0.4.0–v0.5.1); next is the rest of production-grade deploys.

- • **TLS termination at the ingress proxy** — ACME + custom domains so the proxy can own certs; today the guest terminates its own TLS via SNI passthrough.
- • **Native cloud-registry auth** — ECR `GetAuthorizationToken` / GCP / Azure token exchange (and instance-identity creds), so cloud registries "just work" without re-feeding a short-lived token.
- • **Volumes.** Persistent block storage decoupled from an instance, so stateful apps (postgres, sqlite) survive a redeploy — the real ceiling of today's stateless re-create model.
- • **PTY / full terminal.** The interactive shell is line-buffered today; a real PTY adds full-screen programs, colors, and Ctrl-C job control.
- • **Pause / freeze-for-forensics.** `crucible pause <id>` freezes a suspicious workload and snapshots it for analysis before you kill it — Firecracker pause + snapshot already exist under the hood; this surfaces them as a security-ops action.
- • **Growable live disk + accounting.** `--disk` sizes the writable rootfs at create today; this adds growing a live sandbox's disk and per-sandbox disk accounting.

### v0.4.x — Hardening & ecosystem

- • **More language profiles** — Rust, Java, Ruby, Swift, C/C++, bash-only, minimal-alpine.
- • **`policy.yaml`.** A single versionable artifact that supersets scoped tokens — quotas, syscall rules, network allowlists, and mount policies.
- • **Per-language seccomp policies.** Hand-tuned syscall allowlists per runtime; generic policies are too loose.
- • **DNS-layer allowlist filtering** and **packet capture on demand** (`crucible sandbox tcpdump …` → a pcap of everything the sandbox did on the network).

### v0.5.2 — Scale out

Same-app horizontal scaling — generalizing the 0→1 wake path into 0→N. (App→app networking already shipped in v0.5.1.)

- • **Multiple instances behind the proxy.** Run N copies of an app with round-robin / least-connection load balancing. Fork stamps each warm instance from a snapshot, so scaling up is cheap — a better cold-scale story than copying a full VM.
- • **Autoscaling.** Scale on request rate / concurrency between a `min` (which may be **0**, unifying with scale-to-zero) and a `max` — so autoscale-from-zero is just "scale 0→N" on top of the wake primitive that already ships.
- • **Per-app egress allowlists for internal calls.** CIDR/port scoping on top of the `--can-call` grant, for tighter multi-service policies.

### v0.5.x — Observability

First-class, exportable telemetry (v0.5.0–v0.5.1 ship only the wake-latency / sleep-count / internal-request slices):

- • **Per-app request metrics.** RPS, latency percentiles, and error rates per app on `/metrics`, plus reference Grafana dashboards as code.
- • **OpenTelemetry export.** Every lifecycle event, exec, snapshot, and sleep/wake as OTel spans with stable semantic conventions; OTLP to Jaeger / Tempo / Datadog / Honeycomb / Grafana.
- • **Metering.** vCPU-seconds and RAM-GiB-hours that count slept time at ~0 — the economic proof that scale-to-zero actually saves.
- • **Syscall tracing.** Optional per-sandbox syscall log via ptrace or eBPF — expensive to enable, valuable when you need to know exactly what an agent's code did.
- • **Filesystem diff.** `crucible fs diff sbx_…` shows every file created, modified, or deleted vs. the starting rootfs.
- • **Record and replay.** Capture a full execution trace (stdin/stdout, env, filesystem writes, network bytes) and replay it deterministically in a new sandbox.

### v0.6 — Fork trees

Make parallel agent exploration a first-class workflow, not just a primitive. Fork lineage (v0.2.0) and the TUI tree view are the groundwork.

- • **Fork tree API.** Explicit parent/child relationships between snapshots, with depth limits and branch pruning.
- • **Scoring hooks.** Attach a scoring function to a fork tree; crucible prunes the lowest-scoring branches and keeps exploring the best. Beam search for code.
- • **Shared memory reads.** Children forked from the same snapshot read shared pages without duplication, cutting memory cost per fork.

### Longer term

Directions that matter once the core is solid. Not committed to a version or order yet.

- • **Persistent workspaces & richer interactive sessions.** Bidirectional stdin ships (`crucible shell` / `exec -i`) and a full PTY is planned (see Next); what's left is longer-lived named workspaces an agent reattaches to, and first-class REPL / language-server ergonomics on top of the shell.
- • **First-party agent integrations.** Native hooks and ready-made examples for Claude Code, Cursor, and common agent frameworks, building on the MCP server, plus a typed SDK (Python/TS) so the fork/snapshot workflow isn't hand-rolled over HTTP.
- • **Snapshot sharing.** A registry for warm setup-snapshots — boot a "Django project, dependencies installed" snapshot and run against it instantly.
- • **Published regression benchmarks.** The harness ships (`make bench`, [benchmarks.md](benchmarks.md)); tracking cold-start / fork-latency / throughput numbers over releases in CI is the remainder.
- • **Stable API + external security audit.** A versioned API with a deprecation policy and a published third-party audit — the bar for `v1.0`.
- • **WASM profiles.** WebAssembly sandboxes alongside VM sandboxes, for workloads where full-VM isolation is overkill.
