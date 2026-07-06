# Roadmap

This doc lists what is planned, roughly in order. Each section is a coherent milestone — items inside a section are the ones expected to land together. Checkboxes reflect the current state: anything `[x]` is actually working on `main`; anything `[ ]` is not. Nothing is called "done" until it ships.

For the motivation and design principles behind these choices, see [VISION.md](VISION.md).

---

## v0.1 — Core runtime *(in progress)*

The minimal usable thing: boot a sandbox, run a command inside it, get a structured result back, and fork it cheaply.

**Working today:**

- [x] Firecracker orchestration in Go (boot a microVM via the Firecracker API), under the jailer
- [x] HTTP API: create / list / inspect / delete sandboxes, exec, snapshot, fork
- [x] Snapshot + fork primitives — a fork serves guest memory lazily from the snapshot via `userfaultfd` (no per-fork RAM copy)
- [x] Clone-safety — per-fork kernel RNG reseed and machine-identifier rotation, applied before a fork is reachable
- [x] Default-deny networking — each sandbox in its own netns, egress limited to a DNS-proxy hostname allowlist with range-filtered resolved addresses
- [x] Per-request resource ceilings (vCPU, memory, fork fan-out) plus a per-sandbox lifetime and a per-exec deadline
- [x] Structured execution record per exec (exit code, timing, signal, timeout/OOM flags, and CPU/memory/page-fault/context-switch/IO usage)
- [x] Durable sandbox registry with reconcile-on-restart
- [x] Structured JSON lifecycle logs

**Still planned for v0.1:**

- [ ] Pre-baked rootfs profiles: `base`, `python`, `node`, `go`
- [ ] Host-side cgroup quotas wired to defaults (plumbing exists; not applied by default today)
- [ ] Prometheus `/metrics` endpoint — core operational metrics (`sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds`)
- [ ] Python SDK
- [ ] Install script + systemd unit

## v0.2 — Policy, profiles, and language expansion

Extend language coverage and give operators real policy control.

- Language profiles: Rust, Java, Ruby, Swift, C/C++, bash-only, minimal-alpine
- **Per-language seccomp policies.** Hand-tuned syscall allowlists for Python, Node, Go, Rust runtimes. Generic policies are too loose; per-language is the right granularity.
- **Custom rootfs builder.** `crucible rootfs build ./Dockerfile` produces a Firecracker-compatible rootfs image you can use as a custom profile. Most teams will want this.
- **Policy files.** `policy.yaml` declares quotas, syscall rules, network allowlists, and mount policies as a single versionable artifact.
- **DNS filtering.** Network allowlists expand to hostname-based rules, enforced at the DNS layer (not just IP).
- **Packet capture on demand.** `crucible sandbox tcpdump sbx_7k2m` gives you a pcap of everything the sandbox did on the network. Useful for debugging, essential for security review.

## v0.3 — Observability and debugging

Take the v0.1 primitives and turn them into first-class integrations.

- **OpenTelemetry export.** Every sandbox lifecycle event, every exec, every snapshot emitted as OTel spans with stable semantic conventions. OTLP exporter configured via standard env vars. Goes to Jaeger, Tempo, Datadog, Honeycomb, Grafana — anywhere that speaks OTLP.
- **Prometheus histograms.** Upgrade the v0.1 `/metrics` endpoint with proper histograms for latency-sensitive operations, plus reference Grafana dashboards as code.
- **Syscall tracing.** Optional per-sandbox syscall log via ptrace or eBPF. Expensive to enable; valuable when you need to understand exactly what an agent's code did.
- **Filesystem diff.** `crucible fs diff sbx_7k2m` shows every file created, modified, or deleted inside the sandbox vs. its starting rootfs.
- **Record and replay.** Capture a full execution trace (stdin/stdout, env vars, filesystem writes, network bytes). Replay deterministically inside a new sandbox for debugging. Essential for reproducing agent failures.

## v0.4 — Fork trees

Make parallel agent exploration a first-class workflow, not just a primitive.

- **Fork tree API.** Explicit parent/child relationships between snapshots. Explore with depth limits and branch pruning.
- **Tree visualization.** `crucible tree show sbx_7k2m` renders the fork genealogy of a sandbox as a tree.
- **Scoring hooks.** Attach a scoring function to a fork tree; crucible prunes the lowest-scoring branches and continues exploring the most promising ones. Beam search for code.
- **Shared memory reads.** Children forked from the same snapshot can read shared pages without duplication, cutting memory cost per fork substantially.

## Longer term

Directions that matter once the core is solid. Not committed to a version or a fixed order yet.

- **Interactive and streaming execution.** Streaming stdout/stderr (SSE or gRPC), bidirectional stdin so agents can drive REPLs and language servers, and persistent workspaces that survive beyond a single exec.
- **First-party agent integrations.** Native hooks for Claude Code, Cursor, and the common agent frameworks, plus an MCP server so any MCP-compatible agent can drive crucible directly.
- **Snapshot sharing.** A registry for warm setup-snapshots — boot a "Django project, dependencies installed" snapshot and run against it instantly.
- **Benchmarking harness.** Published regression numbers for cold start, fork latency, and network throughput.
- **Stable API + external security audit.** A versioned API with a deprecation policy, and a published third-party audit — the bar for calling anything `v1.0`.
- **WASM profiles.** WebAssembly sandboxes alongside VM sandboxes, for workloads where full-VM isolation is overkill.
- **Deterministic replay for security research.** Record a sandbox execution at syscall granularity and replay it deterministically to study agent failures or malware.
