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

- [x] **MCP server** (`crucible mcp serve`) — a stdio [Model Context Protocol](https://modelcontextprotocol.io) server so any MCP agent (Claude Code, Cursor, …) drives crucible as native tools, with operator guardrails the agent can't widen. Built on `internal/client`, so an MCP call and the CLI hit the identical path and can't drift ([mcp.md](mcp.md)).
- [x] **Daemon API-key auth** — bearer keys hashed at rest; once any key exists every request must present it, and binding a non-loopback address is refused without keys **and** TLS ([SECURITY.md](../SECURITY.md)).

### v0.1.3 — Scoped / policy tokens

- [x] Bind a key to a policy the **daemon** enforces — allowed operations, an egress ceiling, a profile allowlist, resource caps, and an expiry — so a handed-out key is worthless beyond its bounds. `crucible policy validate/show`, `GET /whoami` ([policy.md](policy.md)).

### v0.2.0 — TUI + fork lineage

- [x] **TUI** (`crucible tui`) — a live terminal dashboard: running sandboxes, the fork tree, and interactive streaming `exec`, with create/snapshot/fork/delete gated on the token's scope. A thin consumer of `internal/client` ([tui.md](tui.md)).
- [x] **Fork lineage on the API** — `source_snapshot_id` records which snapshot a sandbox was forked from, so the fork genealogy is reconstructable by any client (this is what the tree view draws).

### v0.3.0 — The safe `docker run` for untrusted/AI code *(current)*

- [x] **OCI image boot** — `crucible run <image>` boots an unmodified image's entrypoint in a microVM; `crucible build` builds a Dockerfile and loads it into the store (daemon stays Docker-free). Publish host ports with `-p`.
- [x] **Interactive shell** — `crucible shell <id>` / `sandbox exec -i`: a real long-lived `/bin/sh` over a hijacked full-duplex vsock stream (state persists; line-buffered, **no PTY**). The TUI gains a **scrollback + `tab`-to-shell** session view.
- [x] **`--disk`** per-sandbox writable sizing (`resize2fs` the clone, never the shared image); top-level **`stop`/`rm`** ops verbs; durable **`logs`**.
- [x] **MCP for the wedge** — `image`/`pull`/`publish`/`disk_mib` on `create_sandbox`/`run`, plus `logs` and `stop_sandbox` tools ([mcp.md](mcp.md)).
- [x] **Complete orphan reaping** — startup sweeps live orphan processes and empty orphan cgroups; a killed daemon leaves no lingering firecracker.
- **Ephemeral contract:** running sandboxes do **not** survive a daemon restart — durability is v0.4.

## Planned

### v0.3.x — Copy files into/out of a sandbox (`crucible cp`) *(next)*

The most-requested gap in the agentic iteration loop: get *your* files in and run them, with no image build and no Dockerfile. A small follow-up to v0.3.0, independent of the v0.4 work.

- • **`crucible cp <local> <sbx>:<path>`** — copy a file or directory into a running sandbox (tar over vsock; the safe-*copy* model, not a host bind-mount, so the guest gets files it can't use to reach back). Turns any sandbox into a scratch workspace: `run python:3.12` → `cp ./script.py <sbx>:/app/` → `exec`/`shell`. Pairs with snapshot + fork — `cp` a project in once, then fork N variations that all inherit it.
- • **`crucible cp <sbx>:<path> <local>`** — copy artifacts back out. The security-sensitive direction: the tar comes from untrusted guest code, so host-side extraction is tar-slip-safe and size-bounded (adversarial-input handling).
- • **MCP `write_files` / `read_file`** — the "drop code in and run it" primitive for agents, alongside `exec`.

### v0.4 — Durable, long-lived workloads

v0.3.0 sandboxes are consciously ephemeral — a daemon restart drops running VMs. v0.4 is about making a workload something you *manage over time* rather than only spin up and tear down.

- • **An app model that survives restart.** Promote a sandbox to a named, durable app whose desired state is reconciled — so the *running workload* (not just a registry record) comes back after a daemon restart or host reboot.
- • **Health checks + restart policy.** The supervisor already restarts a crashed entrypoint on command; v0.4 adds daemon-side health probing and a declarative restart policy (always / on-failure / never) so an app self-heals.
- • **Reach an app by name.** Routing that keys on app identity rather than an ephemeral sandbox id, so a stable name is how you address a workload.
- • **Private / authenticated registry pull.** v0.3.0 pulls anonymous public images; v0.4 adds credentialed pulls from private registries (ghcr.io, ECR, …).
- • **PTY / full terminal.** The interactive shell (`crucible shell`) is line-buffered today; v0.4 adds a real PTY for full-screen programs, colors, and Ctrl-C job control.
- • **Pause / freeze-for-forensics.** `crucible pause <id>` freezes a suspicious workload and snapshots it for analysis before you kill it — Firecracker pause + snapshot already exist under the hood; this surfaces them as a security-ops action.
- • **Growable disk + accounting.** `--disk` sizes the writable rootfs at create today; v0.4 adds growing a live sandbox's disk and per-sandbox disk accounting.

### v0.4.x — Hardening & ecosystem

- • **More language profiles** — Rust, Java, Ruby, Swift, C/C++, bash-only, minimal-alpine.
- • **`policy.yaml`.** A single versionable artifact that supersets scoped tokens — quotas, syscall rules, network allowlists, and mount policies.
- • **Per-language seccomp policies.** Hand-tuned syscall allowlists per runtime; generic policies are too loose.
- • **DNS-layer allowlist filtering** and **packet capture on demand** (`crucible sandbox tcpdump …` → a pcap of everything the sandbox did on the network).

### v0.5 — Observability and debugging

Turn the per-exec records into first-class, exportable telemetry.

- • **OpenTelemetry export.** Every lifecycle event, exec, and snapshot as OTel spans with stable semantic conventions; OTLP to Jaeger / Tempo / Datadog / Honeycomb / Grafana.
- • **Prometheus histograms.** Proper latency histograms on the `/metrics` endpoint, plus reference Grafana dashboards as code.
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

- • **Persistent workspaces & richer interactive sessions.** Bidirectional stdin ships (`crucible shell` / `exec -i`) and a full PTY is v0.4; what's left is longer-lived named workspaces an agent reattaches to, and first-class REPL / language-server ergonomics on top of the shell.
- • **First-party agent integrations.** Native hooks and ready-made examples for Claude Code, Cursor, and common agent frameworks, building on the MCP server, plus a typed SDK (Python/TS) so the fork/snapshot workflow isn't hand-rolled over HTTP.
- • **Snapshot sharing.** A registry for warm setup-snapshots — boot a "Django project, dependencies installed" snapshot and run against it instantly.
- • **Published regression benchmarks.** The harness ships (`make bench`, [benchmarks.md](benchmarks.md)); tracking cold-start / fork-latency / throughput numbers over releases in CI is the remainder.
- • **Stable API + external security audit.** A versioned API with a deprecation policy and a published third-party audit — the bar for `v1.0`.
- • **WASM profiles.** WebAssembly sandboxes alongside VM sandboxes, for workloads where full-VM isolation is overkill.
