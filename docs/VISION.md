---
title: Vision
description: "What crucible is for and why it is shaped this way: the stable motivation and principles behind the runtime."
---

# Vision

This doc is the "what crucible is for, and why it's shaped the way it is." The motivation and principles are stable; details move. For what's functional today, see the top-level [README](../README.md).

---

## The problem

AI coding agents write code. They also want to *run* that code — to check if it compiles, run the tests they just wrote, try three approaches in parallel, iterate until it looks right.

Today, agent builders solve this one of three ways:

1. **Raw Docker containers.** Shared kernel, porous isolation, no checkpoint/fork. Fine for hobbyists, wrong for untrusted code.
2. **Pay for a hosted sandbox service.** Fast to integrate, but you're locked in, your agent traces leave your infra, and cost scales linearly with usage.
3. **Roll your own Firecracker stack.** True isolation, fast boots, but weeks-to-months of operational work to get right. Most teams punt.

**crucible is option four:** a single-binary, self-hosted sandbox runtime with the operational defaults baked in. Firecracker under the hood, a clean HTTP API on top, and a small set of opinionated features — fast snapshot restore, fork/checkpoint, default-deny networking — tuned for coding-agent workloads.

The same need shows up any time you want to **run code you don't trust** — a random repo, an unaudited dependency, an unmodified Docker image — not just an agent's snippets. That's why crucible boots OCI images directly (`crucible run <image>`) and reads as a **safe `docker run`** for untrusted code: the moment you'd `docker run` something you're not sure about, reach for `crucible run` and get a real VM boundary and default-deny egress instead of a shared kernel.

## What crucible is

A single Go binary you run on a Linux host. It speaks HTTP: you hand it a command, it gives you back stdout, stderr, an exit code, and a structured execution record. Behind the scenes it boots Firecracker microVMs, restores them from snapshots, forks them cheaply, isolates their networking, and cleans up when you're done. Drive it with the CLI, a live TUI dashboard, or an MCP server for agents — all thin clients over the same API, so they can't drift.

The interesting features are the ones you don't have to ask for:

- **Fast cold starts** via Firecracker snapshot restore + lazy `userfaultfd` memory — not a cold boot.
- **Fork/checkpoint as first-class primitives** — run setup once, snapshot, fork N parallel children from that point.
- **Clone-safety** — each fork wakes with fresh kernel RNG state and rotated machine identifiers, so forks don't silently share UUIDs, secrets, or entropy.
- **Locked-down network by default** — no egress except an explicit hostname allowlist, with resolved addresses range-filtered so a guest can't reach cloud-metadata or private ranges.
- **Per-request resource ceilings** — vCPU count, memory, and fork fan-out are bounded at the API boundary; every sandbox has a lifetime timeout and every exec a deadline.
- **Structured per-exec results (exit, timing, signal, OOM, and CPU/memory/I/O usage), durable per-sandbox activity logs (`crucible logs` — command + output survive the sandbox), JSON-formatted daemon logs, and a Prometheus `/metrics` endpoint** today; OpenTelemetry export is on the roadmap.

## How it compares

| | Docker | gVisor | Firecracker (raw) | E2B (hosted) | **crucible** |
|---|---|---|---|---|---|
| True VM isolation | ❌ | Partial | ✅ | ✅ | ✅ |
| Fast cold start | ✅ | ✅ | With snapshots | ✅ | ✅ (snapshot + lazy memory) |
| Fork / checkpoint | ❌ | ❌ | Primitive only | Limited | **First-class** |
| Self-hosted single binary | ✅ | ❌ | ❌ | ❌ | ✅ |
| No cloud lock-in | ✅ | ✅ | ✅ | ❌ | ✅ |
| Opinionated defaults for AI agents | ❌ | ❌ | ❌ | ✅ | ✅ |

If you need a primitive, use Firecracker. If you want to hand somebody a credit card, use E2B. crucible is for teams that want Firecracker-grade isolation without Firecracker-grade operational pain — self-hosted, with defaults already tuned for agent workloads.

## Core concepts

**Sandbox.** A Firecracker microVM with its own kernel, rootfs, and network policy. Lives as long as you need it; cleaned up on timeout or explicit delete.

**Rootfs.** The filesystem image a sandbox boots from. Pre-baked language profiles (`base`, `python`, `node`, `go`) ship, and you can boot any **unmodified OCI image** (`crucible run <image>` pulls + converts it) or build one from a Dockerfile straight into the store (`crucible build`).

**Snapshot.** A frozen sandbox state — memory, registers, disk — captured at a point in time. Restores without a cold boot, and is the basis for forking.

**Fork.** A new sandbox created from a snapshot's state. Cheap: guest memory is served lazily from the snapshot file via `userfaultfd` (no per-fork RAM copy), and the rootfs is cloned copy-on-write. Run setup once, snapshot, then fork many children — each starts where the parent left off and runs independently.

**Execution.** One command run inside a sandbox. stdout and stderr stream back as frames; the final frame carries a structured record:

```json
{
  "exit_code": 0,
  "duration_ms": 247,
  "signal": "",
  "timed_out": false,
  "oom_killed": false,
  "usage": {
    "cpu_user_ms": 180,
    "cpu_sys_ms": 40,
    "peak_memory_bytes": 35389440,
    "page_faults_major": 2,
    "context_switches_involuntary": 14,
    "io_read_bytes": 0,
    "io_write_bytes": 0
  }
}
```

**Policy.** The ruleset governing a sandbox — resource ceilings, timeouts, network allowlist. Sane defaults; override per-sandbox if needed.

## The fork/checkpoint primitive

This is the feature that makes crucible different, so it's worth explaining.

Coding agents frequently want to **try multiple approaches to the same problem in parallel** — generate three candidate patches, run each against the test suite, keep the one that passes most tests. Naïvely, that means three full sandboxes, each installing dependencies and setting up fixtures — expensive and slow.

crucible turns this into two steps:

1. Run setup once in a parent sandbox: `git clone`, `pip install`, boot the app, load fixtures.
2. Snapshot. Now fork N children from that snapshot. Each child starts at exactly the point the parent finished setup — memory, disk, and loaded processes intact. Dependencies are already installed; the app is already warm.

Illustrative agent workflow (pseudocode — a Python SDK is planned; today you'd drive this through the CLI or the HTTP API):

```python
with c.sandbox(profile="python") as parent:
    parent.exec(["git", "clone", repo_url, "/app"])
    parent.exec(["pip", "install", "-r", "/app/requirements.txt"])
    snapshot = parent.snapshot()

    results = []
    for patch in candidate_patches:           # try N patches in parallel
        with snapshot.fork() as child:
            child.write_file("/app/patch.diff", patch)
            child.exec(["git", "apply", "/app/patch.diff"])
            results.append((patch, child.exec(["pytest", "/app/tests"])))

    best = max(results, key=lambda r: r[1].tests_passed)
```

Setup in the parent happens once; forking is cheap (no per-fork RAM copy). This is the pattern every serious coding agent wants and almost nobody has a clean primitive for. That's why it's first-class here.

The `write_file` step above hints at the other half of the iteration loop: **getting your working files into a sandbox without building an image**. A file-copy primitive — `crucible cp ./script.py <sbx>:/app/` — is the near-term roadmap: drop code into a running `python`/`node`/`go` sandbox and run it directly, no Dockerfile round-trip. It's the safe-*copy* model (the guest gets a copy it can't use to reach your host), and it composes with fork — copy a project in once, then fork N variations that all inherit it.

## Design principles

**Firecracker isolation, not namespace trust.** Every sandbox is a separate kernel. Escape attacks have to break out of a VM, not a shared kernel — the same isolation model AWS Lambda and Fargate use.

**Opinionated defaults over flexibility.** The default is: no network, 512 MB memory, 1 CPU, 60 s timeout, loopback-only API. If you need more, you ask. For AI-generated code you want the safety.

**Uniqueness is a correctness property.** Cloning a VM naïvely means every fork shares the source's RNG state, machine-id, and cached secrets — a real hazard for code that generates UUIDs, nonces, or tokens. crucible reseeds and rotates that state per fork, before the fork is reachable.

**Observability is not optional.** Every exec returns a structured result with resource usage — exit code, wall-clock duration, OOM/timeout flags, and CPU/memory/I/O counters — plus a Prometheus `/metrics` endpoint and JSON-formatted logs. Per-sandbox activity now **persists durably** (`crucible logs` — every exec's command + output and each lifecycle event, survivable and tailable after a client detaches or the sandbox is gone), so a detached run or an MCP agent's actions leave a trail. OpenTelemetry export is the near-term roadmap. A sandbox runtime without observability is a black box.

**Fork/checkpoint is a primitive, not an afterthought.** Most runtimes treat snapshot/restore as a perf optimization. crucible treats it as an API surface your agent reasons about and structures its exploration around.

**Self-hosted, single binary, no cloud.** No telemetry, no callbacks home, no account. Download a binary, run it. This matters for data locality, cost control, and basic trust.

**Production-honest.** Documented limitations, no claims the tool hasn't earned. Pre-1.0 means pre-1.0 — see [SECURITY.md](../SECURITY.md) for exactly what is and isn't safe today.

## FAQ

**Why not just use Docker?**
Docker is not a security boundary — it shares a kernel with the host, and container escapes are regularly discovered. For your own code it's fine. For AI-generated code that might include a hallucinated `rm -rf` or a subtle resource-exhaustion attack, you want a real VM boundary.

**Why not gVisor?**
gVisor is genuinely good and more battle-tested than crucible. Two reasons to prefer Firecracker: compatibility (gVisor's user-space kernel doesn't implement every syscall, which breaks the esoteric code agents sometimes produce) and performance (Firecracker is closer to raw Linux for CPU-heavy work). If gVisor's model suits you, it's a valid choice — crucible is a deliberate different trade-off.

**Why Firecracker over QEMU or Cloud Hypervisor?**
Firecracker's attack surface is intentionally minimal — no USB, no PCI, no legacy devices — it boots in ~125 ms, and it powers AWS Lambda and Fargate, so it's battle-tested at scale.

**Does this work on macOS / Windows?**
The **daemon** is Linux-only — Firecracker requires KVM, which requires Linux — but the **client** (the CLI, TUI, and `crucible mcp serve`) is cross-platform, because everything but the daemon is a thin HTTP client. So the macOS / Windows path is: run the daemon on a Linux host (a cloud VM, a homelab box, a Linux desktop) and install the client on your Mac, pointed at it with `--addr`/`CRUCIBLE_ADDR`. Running the daemon *locally* on a Mac would mean a nested Linux VM, and nested virtualization on Apple Silicon is unreliable — so the remote-daemon + local-client split is the recommended path, not a workaround.

**Is this production-ready? Can I run untrusted multi-tenant code?**
Not yet. It's pre-1.0 and single-operator: loopback by default, with optional bearer-key auth and daemon-enforced scoped tokens for remote access — but a pre-release, not a hardened multi-tenant platform. See [SECURITY.md](../SECURITY.md) for the current isolation model and its limits.

**How does this compare to E2B?**
E2B is a hosted service — great for "hand somebody a credit card and be done." crucible is self-hosted — better if you need data locality, cost control, or you don't want your agent traces leaving your infra. Similar runtime goals; opposite product shape.

## Origins

crucible started as frustration with the options for running agent-generated code safely: the choices were all wrong in different ways, and the gap between "raw Firecracker" and "pay somebody" was too big to leave alone. It's built by engineers who work on systems, isolation, and AI-agent infrastructure.
