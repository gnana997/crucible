# Vision

This doc is the "what crucible is for, and why it's shaped the way it is." It's stable — the details move, but the motivation and principles don't. For a list of what's built vs. what's planned, see [ROADMAP.md](ROADMAP.md). For what's functional today, see the top-level [README](../README.md).

---

## The problem

AI coding agents write code. They also want to *run* that code — to check if it compiles, to run the tests they just wrote, to try three approaches in parallel, to iterate on output until it looks right.

Today, agent builders solve this one of three ways:

1. **Raw Docker containers.** Shared kernel, porous isolation, slow cold starts, no checkpoint/fork. Fine for hobbyists, wrong for anything with untrusted code.
2. **Pay for a hosted sandbox service.** Fast to integrate, but you're locked in, your agent traces leave your infra, and cost scales linearly with agent usage — which can get expensive fast.
3. **Roll your own Firecracker stack.** True isolation, fast boots, but two months of operational work to get right. Most teams punt.

**crucible is option four:** a single-binary, self-hosted sandbox runtime with the operational defaults baked in. Firecracker under the hood, a clean HTTP API on top, and a small set of opinionated features — cold-start snapshotting, fork/checkpoint, sane network policies, built-in observability — tuned specifically for coding agent workloads.

## What crucible is

```
┌──────────────────────────────┐
│   Your coding agent          │
│   (Claude Code, Cursor,      │
│    custom agent, CI, ...)    │
└───────────────┬──────────────┘
                │ HTTP / gRPC
                ▼
┌──────────────────────────────┐
│   crucible (single Go binary)│
│   ┌────────────────────────┐ │
│   │ orchestrator           │ │
│   │ ├─ snapshot manager    │ │
│   │ ├─ fork scheduler      │ │
│   │ ├─ quota enforcer      │ │
│   │ └─ observability       │ │
│   └───────────┬────────────┘ │
│               │              │
│   ┌───────────▼────────────┐ │
│   │ Firecracker microVMs   │ │
│   │ VM  VM  VM  VM  VM     │ │
│   └────────────────────────┘ │
└──────────────────────────────┘
```

A single Go binary you run on a Linux host. It speaks HTTP. You hand it a command, it gives you back stdout, stderr, exit code, and a clean observability record. Behind the scenes it's booting Firecracker microVMs from pre-baked snapshots, enforcing CPU/memory/disk quotas, and cleaning up when you're done.

The interesting features are the ones you don't have to ask for:

- **Sub-second cold starts** via Firecracker snapshot restore, not cold boot.
- **Fork/checkpoint as first-class primitives** — run setup once, snapshot, fork N parallel children from that point.
- **Locked-down network by default** — no egress except for an explicit allowlist (`pypi.org`, `registry.npmjs.org`, etc.).
- **Hard resource quotas** — CPU, memory, disk, wallclock, syscall count. Every sandbox has an enforced ceiling.
- **Observability-native from v0.1** — a structured execution record per exec, a `/metrics` endpoint in Prometheus text format, and JSON-formatted lifecycle logs. Full OTel spans and first-class integrations (Datadog, Honeycomb, Jaeger, Tempo) follow in v0.2.

## How it compares

| | Docker | gVisor | Firecracker (raw) | E2B (hosted) | **crucible** |
|---|---|---|---|---|---|
| True VM isolation | ❌ | Partial | ✅ | ✅ | ✅ |
| Sub-second cold start | ✅ | ✅ | With snapshots | ✅ | ✅ |
| Fork / checkpoint | ❌ | ❌ | Primitive only | Limited | **First-class** |
| Self-hosted single binary | ✅ | ❌ | ❌ | ❌ | ✅ |
| Built-in quotas + observability | Partial | Partial | DIY | ✅ | ✅ (primitives v0.1, full integrations v0.2) |
| No cloud lock-in | ✅ | ✅ | ✅ | ❌ | ✅ |
| Opinionated defaults for AI agents | ❌ | ❌ | ❌ | ✅ | ✅ |

If you need a primitive, use Firecracker. If you want to hand somebody a credit card, use E2B. crucible is for teams that want Firecracker-grade isolation without Firecracker-grade operational pain, self-hosted, with defaults already tuned for agent workloads.

## Core concepts

**Sandbox.** A Firecracker microVM with its own kernel, rootfs, resource quotas, and network policy. Lives for as long as you need it; cleaned up automatically on timeout or explicit delete.

**Rootfs profile.** The pre-baked filesystem image a sandbox boots from. Ships with `python`, `node`, `go`, and `base` profiles; custom profiles via `crucible rootfs build`.

**Snapshot.** A frozen sandbox state — memory, registers, disk — captured at a point in time. Restores in ~150 ms. The basis for fast cold starts and forking.

**Fork.** A new sandbox created from a snapshot's state. Cheap (copy-on-write). You run setup once, snapshot, then fork 100 parallel children — each starts where the parent left off, runs independently, and costs almost nothing.

**Execution.** One command run inside a sandbox. Returns a structured record:

```json
{
  "exit_code": 0,
  "stdout": "...",
  "stderr": "...",
  "duration_ms": 247,
  "peak_memory_mb": 34,
  "peak_cpu_pct": 62,
  "syscall_count": 1847,
  "network_bytes_out": 0,
  "network_bytes_in": 0,
  "killed_by": null
}
```

**Policy.** The ruleset that governs a sandbox — quotas, timeouts, allowed syscalls, network allowlist. Sane defaults; override per-sandbox if needed.

## The fork/checkpoint primitive

This is the feature that makes crucible different, so it's worth explaining.

Coding agents frequently want to **try multiple approaches to the same problem in parallel**. Generate three candidate patches, run each against the test suite, keep the one that passes most tests. In a naïve setup this means spinning up three full sandboxes, each installing dependencies, each setting up fixtures — expensive and slow.

crucible turns this into two steps:

1. Run setup once in a parent sandbox: `git clone`, `pip install -r requirements.txt`, boot the app, load fixtures.
2. Snapshot. Now fork N children from that snapshot. Each child starts at exactly the point the parent finished setup, with all memory state, disk state, and loaded processes intact. Dependencies are already installed. The app is already warm.

Applied to agent workflows:

```python
with c.sandbox(profile="python") as parent:
    parent.exec(["git", "clone", repo_url, "/app"])
    parent.exec(["pip", "install", "-r", "/app/requirements.txt"])
    parent.exec(["cd", "/app"])

    snapshot = parent.snapshot()

    # Agent generates 5 candidate patches. Try them all in parallel.
    results = []
    for patch in candidate_patches:
        with snapshot.fork() as child:
            child.write_file("/app/patch.diff", patch)
            child.exec(["git", "apply", "/app/patch.diff"])
            result = child.exec(["pytest", "/app/tests"])
            results.append((patch, result))

    best = max(results, key=lambda r: r[1].tests_passed)
```

The forking itself takes ~150 ms. The setup in the parent happens once. Running 20 patch variations in parallel is ~3 seconds of orchestration plus whatever your tests take.

This is the pattern every serious coding agent wants and almost nobody has a clean primitive for. That's why it's first-class here.

## Design principles

**Firecracker isolation, not namespace trust.** Every sandbox is a separate kernel. Escape attacks have to break out of a VM, not out of a shared kernel. This is the same isolation model AWS Lambda and Fargate use.

**Opinionated defaults over flexibility.** The default policy is: no network, 512 MB memory, 1 CPU, 60 s timeout, strict seccomp. If you need more, you ask. This trades configurability for safety — and for AI-generated code you want the safety.

**Observability is not optional.** Every execution produces a structured record with resource usage, syscall counts, and network bytes. From v0.1 you get a Prometheus `/metrics` endpoint and JSON-formatted lifecycle logs out of the box. Full OTel spans and first-class integrations land in v0.2. A sandbox runtime without observability is a black box, and black boxes don't belong in production.

**Fork/checkpoint is a primitive, not an afterthought.** Most sandbox runtimes treat snapshot/restore as a perf optimization. crucible treats it as an API surface — it's something your agent reasons about, uses explicitly, and structures its exploration around.

**Self-hosted, single binary, no cloud.** No telemetry, no callbacks home, no account required. You download a binary, run it, and it works. This matters for regulated industries, for cost control, and for basic trust.

**Production-honest.** Documented failure modes, known limitations in the README, no claims the tool hasn't earned. `v0.1` means v0.1, not "works great, trust me."

## FAQ

**Why not just use Docker?**
Docker is not a security boundary. It shares a kernel with the host; container escapes are regularly discovered and patched. For your own code, Docker is fine. For code written by an AI agent that might include hallucinated `rm -rf` calls or subtle resource exhaustion attacks, you want a real VM boundary.

**Why not gVisor?**
gVisor is genuinely good and more production-tested than crucible will be for a while. Two reasons to prefer Firecracker: (1) compatibility — gVisor's user-space kernel doesn't implement every syscall, which can break esoteric code that AI agents sometimes produce; (2) performance — Firecracker is closer to raw Linux for CPU-heavy workloads. If gVisor's security model suits you and you're running mostly standard code, it's a valid choice. crucible's isolation model is a deliberate different trade-off.

**Why Firecracker over QEMU or Cloud Hypervisor?**
Firecracker's attack surface is intentionally minimal — no USB, no PCI, no legacy device support. Boot time is ~125 ms. It powers AWS Lambda and Fargate, so it's battle-tested at extreme scale. Cloud Hypervisor becomes relevant once we need GPU passthrough (v0.8).

**Does this work on macOS?**
Firecracker requires KVM, which requires Linux. On macOS, run crucible inside a Linux VM via Lima, UTM, or Docker Desktop. Native macOS support is not planned — Apple's Virtualization framework is a different API and would effectively be a second product.

**What about Windows?**
No plans. Firecracker is Linux-only.

**Is this production-ready?**
v0.1 is not. v0.3 should be, for low-risk internal use. v1.0 is the production-ready milestone with a security audit and stability commitment. Use crucible in production before v1.0 at your own risk.

**How does this compare to E2B?**
E2B is a hosted service — great for "I want to hand somebody a credit card and be done." crucible is self-hosted — better if you need data locality, cost control at scale, or you don't want your agent traces leaving your infra. The runtime goals are similar; the product shape is opposite.

**Can I run untrusted user code with this?**
At v0.1, no — too many rough edges. At v1.0, yes, with a careful configuration. Multi-tenant production use cases (e.g., a "run any code" web app) need v0.5 at minimum for tenant isolation and v1.0 for the security review.

## Who built this

I'm Gnana — senior platform engineer working on distributed systems, observability, and AI agent infrastructure. crucible started as a frustration with the options available for running agent-generated code safely. The existing choices were all wrong in different ways, and the OSS gap between "raw Firecracker" and "pay somebody" was too big to leave alone.

Writing about platform engineering, distributed systems, and agent infra: [gnana.dev](https://gnana.dev)
