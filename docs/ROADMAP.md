# Roadmap

This doc lists what is planned, version by version. Each section describes a coherent milestone — items inside a section are the ones expected to land together. Checkboxes reflect the current state. Anything `[x]` is actually working on `main`; anything `[ ]` is not. Nothing is called "done" until it ships.

For the motivation and design principles behind these choices, see [VISION.md](VISION.md).

---

## v0.1 — Core runtime *(in progress)*

The minimal usable thing. Boot a sandbox, run a command inside it, get structured output back, with observability baked in from day one.

- [ ] Firecracker orchestration in Go (boot a microVM via the Firecracker API)
- [ ] Pre-baked rootfs profiles: `base`, `python`, `node`, `go`
- [ ] HTTP API: create, exec, snapshot, fork, delete
- [ ] Hard resource quotas: CPU, memory, disk, wallclock
- [ ] Snapshot + fork primitives
- [ ] Default-deny network with per-sandbox allowlist
- [ ] Structured execution record per exec (exit code, timing, peak memory/CPU, syscall count, network bytes)
- [ ] Prometheus `/metrics` endpoint — core operational metrics (`sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds`, `quota_violations_total`)
- [ ] Structured JSON lifecycle logs via `--log-format=json`
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

## v0.3 — Full observability and debugging

Take the v0.1 primitives and turn them into first-class integrations.

- **OpenTelemetry export.** Every sandbox lifecycle event, every exec, every snapshot emitted as OTel spans with stable semantic conventions. OTLP exporter configured via standard env vars. Goes to Jaeger, Tempo, Datadog, Honeycomb, Grafana — anywhere that speaks OTLP.
- **Prometheus histograms with tenant labels.** Upgrade the v0.1 `/metrics` endpoint with proper histograms for latency-sensitive operations, cardinality-safe labeling for multi-tenant deployments, and reference Grafana dashboards as code.
- **Syscall tracing.** Optional per-sandbox syscall log via ptrace or eBPF. Expensive to enable; valuable when you need to understand exactly what an agent's code did.
- **Filesystem diff.** `crucible fs diff sbx_7k2m` shows every file created, modified, or deleted inside the sandbox vs. its starting rootfs.
- **Record and replay.** Capture a full execution trace (stdin/stdout, env vars, filesystem writes, network bytes). Replay deterministically inside a new sandbox for debugging. Essential for reproducing agent failures.

## v0.4 — Fork trees

Make parallel agent exploration a first-class workflow, not just a primitive.

- **Fork tree API.** Explicit parent/child relationships between snapshots. Explore with depth limits and branch pruning.
- **Tree visualization.** `crucible tree show sbx_7k2m` renders the fork genealogy of a sandbox as a tree.
- **Scoring hooks.** Attach a scoring function to a fork tree; crucible prunes the lowest-scoring branches and continues exploring the most promising ones. Beam search for code.
- **Shared memory reads.** Children forked from the same snapshot can read shared pages without duplication, cutting memory cost per fork substantially.

## v0.5 — Multi-tenant mode

Get ready for being run by more than one user at a time.

- **API key auth.** Per-tenant API keys with scoped permissions.
- **Tenant quotas.** Global resource ceilings per tenant: max concurrent sandboxes, max memory, max CPU time per hour.
- **Isolation between tenants.** Separate Firecracker network namespaces, separate filesystem roots, separate snapshot storage.
- **Audit log.** Every API call, sandbox lifecycle event, and policy decision logged to an append-only store.
- **Rate limiting.** Per-tenant request rate limits with graceful degradation under pressure.

## v0.6 — Kubernetes operator

Run crucible the way modern infra teams run everything else.

- **`crucible-operator`.** Custom resources: `Sandbox`, `Snapshot`, `ExecutionRequest`.
- **Pod-per-sandbox or pool mode.** Pod-per-sandbox for strong isolation; pool mode for throughput (pre-warmed VMs kept ready).
- **HPA integration.** Scale the sandbox pool based on queue depth.
- **PV-backed snapshot storage.** Snapshots survive node restarts; portable across hosts.
- **NetworkPolicy integration.** Honor cluster-level network rules in addition to per-sandbox policy.

## v0.7 — Distributed scheduler

Scale crucible across a fleet of hosts.

- **Multi-host pool.** A scheduler that places sandboxes across N Firecracker hosts.
- **Placement strategies.** Bin-packing, affinity-based, snapshot-cache-aware. (If a snapshot is already cached on host 3, run the fork there.)
- **Live migration.** Move a running sandbox between hosts. Useful for draining hosts during maintenance.
- **Distributed fork trees.** Trees that span multiple hosts, with efficient snapshot replication.
- **Global quota enforcement.** Tenant quotas respected across the whole fleet, not per-host.

## v0.8 — GPU support

Open the door to ML workloads, not just shell code.

- **GPU passthrough via VFIO.** Firecracker doesn't natively support PCI passthrough; explore using Cloud Hypervisor as an alternative VMM for GPU sandboxes, with the same crucible API on top.
- **GPU quotas and time-slicing.** Share a GPU between multiple sandboxes safely.
- **Pre-baked ML profiles.** `pytorch-gpu`, `jax-gpu`, `llama-cpp` rootfs profiles with CUDA/ROCm pre-installed.
- **Model cache.** Shared read-only model cache mounted into sandboxes to avoid re-downloading.

## v0.9 — Interactive and streaming execution

Make crucible usable for agents that want to have a conversation with a running process.

- **Streaming stdout/stderr.** Server-sent events or gRPC streaming for long-running executions.
- **Interactive stdin.** Bidirectional streaming — agents can drive REPLs, language servers, or interactive CLIs.
- **Language server mode.** Run LSP servers inside a sandbox; proxy their protocol out to the agent. Makes "refactor this project" workflows fast.
- **Persistent workspaces.** Sandboxes that survive beyond a single exec, with filesystem changes persisted to disk for later forking.

## v1.0 — Stability and integrations

By the time we call it 1.0, crucible should be boring, trustworthy, and deeply integrated.

- **Stable API commitment.** Versioned API with deprecation policy.
- **Security audit.** External pen test and audit report published.
- **First-party integrations:** Claude Code, Cursor, LangChain, CrewAI, AutoGen, GitHub Actions.
- **MCP server.** Expose crucible as an MCP tool so Claude Code / Cursor / any MCP-compatible agent can use it natively.
- **Snapshot registry.** Public (or private) registries for sharing snapshots. Boot a Django project setup-snapshot, run commands against it instantly.
- **Benchmarking harness.** Regression tests for cold start, fork latency, syscall overhead, network throughput. Published numbers.
- **Production deployment guide.** From "single host" to "multi-region fleet behind a load balancer." The guide people wish existed for Firecracker.

## Beyond v1.0

Directions worth exploring once the core is rock-solid:

- **WASM profiles.** WebAssembly sandboxes alongside VM sandboxes — useful for extremely cheap, extremely fast workloads where full VM isolation is overkill.
- **Distributed checkpoint registry.** IPFS-style content-addressed snapshots, shareable across organizations.
- **Sandbox federation.** Agents running in sandbox A can call out to crucible to spawn sandbox B, with policy constraints on the chain.
- **Deterministic replay for security research.** Record a full sandbox execution at syscall granularity; replay it deterministically to study malware or agent failures.
