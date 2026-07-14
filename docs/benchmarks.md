---
title: Benchmarks
description: "Reproducible latency measurements from crucible-bench, driving a real daemon through the same typed Go client the CLI and TUI use."
---

# Benchmarks

Reproducible measurements from `crucible-bench`, a harness that drives a real daemon through the `sdk` Go package (the same typed client the CLI, TUI, and MCP server use). It reports latency distributions, fork fan-out scaling, the lazy-memory efficiency of fork, and sandbox density.

![crucible-bench](../demo/bench.gif)

> **Read these as one machine's numbers, not a spec.** They come from a single host, one sandbox size — enough to show the *shape* of the runtime, not a cross-hardware guarantee. Everything you need to re-run is at the bottom.

## The filesystem under `--work-base` is the biggest lever

Fork clones the per-child rootfs copy-on-write. On a **reflink** filesystem (btrfs / XFS `reflink=1`) that clone is O(1) — extents are shared, nothing is copied. On **ext4** (no reflink — still the most common default) it's a **full byte-copy** of the rootfs. Same box, same daemon, same 1 GiB rootfs; only `--work-base`/`--chroot-base` differ:

| | ext4 (common default) | **btrfs (reflink)** |
|---|---|---|
| Fork (warm → child), p50 | 530 ms | **125 ms** — 4.2× faster |
| Fork fan-out @ 64-way | 294 ms/child · 3.4/s | **24 ms/child · 41/s** — ~12× |
| 128-way fork, wall time | 36.4 s | **2.1 s** — ~18× |
| Snapshot, p50 | 644 ms | **466 ms** |
| 64 forks, host RAM | 1.0 GiB | **813 MiB** |
| Proxy wake, p50 | 252 ms | **125 ms** — 2.0× |
| Volume wake, p50 | 240 ms | **178 ms** — 1.3× |

**If you fork a lot, put `--work-base` on btrfs or XFS.** The rest of this page reports both, so you can see the floor (ext4) and the ceiling (reflink). Note that **wake barely cares about the filesystem** (2.0×) — it restores against the live rootfs with lazy memory and copies nothing on the timed path — while **fork/fan-out care enormously** (up to ~18×), because each ext4 child byte-copies a full ~1 GiB rootfs. Marginal *RAM* per fork is similar on both filesystems (~12–16 MiB) — guest pages are shared via `userfaultfd` regardless of the disk; reflink's win is wall-time and I/O, not memory.

## Setup

| | |
|---|---|
| CPU | AMD Ryzen AI 9 HX 370 (24 threads) |
| Kernel | 7.0 · Free RAM ~11.6 GiB (btrfs) / ~19 GiB (ext4) at start, other workloads paused |
| Sandbox | 1 vCPU / 512 MiB, default (1 GiB) rootfs, no network |
| Daemon | measured on **v0.5.4** under jailer, `CRUCIBLE_MAX_FORK` auto-sized to the density target; the **volume wake** row/section re-measured on **v0.6.2**, same host, `redis:alpine` on a volume |
| Samples | 30 per latency op (3 warmup discarded) |
| FS | ext4 (host root) and btrfs (60 GiB loopback) — the daemon's `--work-base`/`--chroot-base` on each; reproduce both with `scripts/bench_reflink.sh` (`FS=btrfs`/`FS=ext4`) |

## Latency

| Operation | ext4 p50 | btrfs p50 | p90 / p99 (btrfs) |
|---|---|---|---|
| Exec roundtrip (`true`, warm) | 2.2 ms | **2.8 ms** | 3.2 / 8.3 ms |
| Cold create (boot → agent ready) | 1.36 s | **1.03 s** | 1.05 / 1.10 s |
| Snapshot (running → on disk) | 644 ms | **466 ms** | 2.16 s / 2.40 s |
| Fork (warm snapshot → child) | 530 ms | **125 ms** | 143 / 157 ms |
| **Proxy wake (request → served)** | **252 ms** | **125 ms** | 153 / 156 ms |
| **Volume wake (asleep → running)** | **240 ms** | **178 ms** | 219 / 257 ms |

**Fork is ~2.6× faster than a cold create even on ext4, and ~8× faster on reflink** — pay the ~1 s boot once, then branch cheaply. **Wake beats a cold create ~8×** (125 ms vs 1.03 s on btrfs) and is nearly storage-independent (see below). Exec overhead is ~2–3 ms (a vsock roundtrip), essentially filesystem-independent. Snapshot's median is ~0.5 s with occasional multi-second tails from writing the 512 MiB memory image through writeback.

## Wake (scale-to-zero)

The v0.5.0 headline: a slept app costs ~zero RAM and wakes on the next request. The **product** number is *proxy wake* — an HTTP request hits a *slept* app through the ingress proxy and is served, with the wake (restore + reseed RNG + step clock) happening inline while the proxy holds the request. `nginx:alpine`, `proxywake` phase:

| Proxy wake (request → served) | ext4 | **btrfs** |
|---|---|---|
| p50 | 252 ms | **125 ms** |
| p90 | 302 ms | 153 ms |
| p99 | 331 ms | 156 ms |

**Wake is ~8× faster than a cold create** (125 ms vs 1.03 s, btrfs) and, unlike fork, only mildly depends on the filesystem (2.0×). That's because wake restores in place against the *live* rootfs with lazy (`userfaultfd`) memory — no rootfs clone, no memory copy on the timed path, cost O(working set) not O(guest RAM). The ext4 penalty is only the page-cache pressure left by the untimed sleep-snapshot. Either way it's comfortably sub-second.

> There is also a `wake` phase (`--phases wake`) that times the bare *sandbox-level* restore-in-place mechanism; it needs a rootfs whose guest agent has `/wake` (OCI images used by `proxywake` always do). Every JSON result carries an `env` block — host, CPU, kernel, memory, and a **live FICLONE reflink probe** of `--reflink-path` — so nobody compares an ext4 number against a btrfs one.

### Stateful (volume) wake — v0.6.2

Before v0.6.2 a volume-backed app woke by **cold boot**: destroy the instance, re-create it, boot the service, run recovery — seconds for a database. v0.6.2 makes a volume app snapshot-sleep and restore in place exactly like a stateless one — the running process comes back from the snapshot with its volume re-attached, nothing copied on the timed path. `redis:alpine` on a volume, `volumewake` phase (`app sleep → app wake → running`):

| Volume wake (asleep → running) | ext4 | **btrfs** |
|---|---|---|
| p50 | 240 ms | **178 ms** |
| p90 | 262 ms | 219 ms |
| p99 | 272 ms | 257 ms |
| daemon restore, p50 (`last_wake_latency_ms`) | 237 ms | **170 ms** |

The volume adds no meaningful overhead over a stateless wake — ~170 ms pure restore on reflink, the same ballpark as the 125 ms non-volume proxy wake — and, like every wake, it barely depends on the filesystem (1.3×), because the restore reads the *live* rootfs + volume drive with lazy (`userfaultfd`) memory instead of copying them. So a self-hosted serverless **postgres** or **redis** comes back in well under a quarter-second on the first connection after it slept, with **no cold boot and no WAL recovery**: the database process is already running in the restored memory, attached to its volume.

## Fork fan-out

Forking *N* children from one snapshot in a single call. Per-child cost falls with batch size as fixed per-fork overhead amortizes — and reflink pulls far ahead as N grows:

| Children | ext4 per-child | btrfs per-child | btrfs throughput |
|---|---|---|---|
| 1 | 621 ms | 173 ms | 5.8/s |
| 4 | 361 ms | 58 ms | 17.2/s |
| 16 | 338 ms | 32 ms | 31.3/s |
| 64 | 294 ms | 24 ms | 41.0/s |
| 128 | 284 ms | **16 ms** | **62.6/s** |

On ext4 each child copies a full 1 GiB rootfs, so per-child cost plateaus around ~285–340 ms and the 128-way batch moves ~128 GiB of disk — it took **36 s vs 2 s** on btrfs. On reflink the clone is O(1), so per-child cost keeps *falling* as fixed overhead amortizes (173 ms → 16 ms), and throughput climbs to ~63 forks/s at 128-way.

## Memory efficiency

The lazy-`userfaultfd` payoff: guest RAM is served on demand from the snapshot's memory file, so forks page in only their working set instead of each copying 512 MiB. Forking **64 children** from one warm snapshot:

| | ext4 | btrfs |
|---|---|---|
| Host RAM consumed | 1.0 GiB (~16.4 MiB/fork) | **813 MiB (~12.7 MiB/fork)** |
| vs naïve *64 × 512 MiB* = 32 GiB | 31× less | **40× less** |
| Wall time | 33.4 s | **1.29 s** |

Marginal RAM per fork is comparable on both filesystems (~13–16 MiB) — the guest's memory is shared read-only via `userfaultfd` on either disk, so what each child adds is just its faulted-in working set plus copy-on-write divergence. btrfs is slightly lighter (no page-cache pressure from rootfs byte-copies) and, crucially, **~26× faster wall-time** (1.3 s vs 33 s) because ext4 also moves 64 GiB of rootfs I/O that reflink skips entirely.

## Density (reflink)

Forking toward a live-sandbox target on btrfs and watching free RAM: this run reached **320 concurrent microVMs** — from ~11.6 GiB free at the start to **7.9 GiB free** at 320 live, ~11 MiB marginal RAM per running VM (its faulted-in working set) — before the first failure.

The stop was **not** RAM exhaustion (7.9 GiB was still free) but a per-fork *restore-readiness* timeout: inside a 64-wide fork batch at that concurrency, one child's guest agent didn't answer `/healthz` within the restore deadline. So 320 is this run's first-failure point, not a hard ceiling — a longer agent-ready deadline (or a smaller fork batch) would push the peak higher.

On reflink, density is bounded by RAM and restore concurrency. On **ext4** it's *disk*-bound instead — each fork writes a full ~1 GiB rootfs, so 320 would move ~320 GiB — which is why this phase is reflink-only (`scripts/bench_reflink.sh FS=ext4` skips density by default). Use reflink for density.

## Reproduce

The one-command runner sets up the target filesystem (a btrfs loopback, or a dir on the ext4 root), stages the rootfs + work dirs on it, starts a daemon, runs `crucible-bench` (including the end-to-end `proxywake` number), stamps the environment + a live reflink probe into the JSON, and tears down:

```bash
make build && make bench                      # builds ./crucible and ./bin/crucible-bench

sudo FIRECRACKER_BIN=… JAILER_BIN=… KERNEL=… ROOTFS=… \
  FS=btrfs scripts/bench_reflink.sh           # reflink numbers → bench-btrfs-*.json
sudo … FS=ext4  scripts/bench_reflink.sh      # ext4 numbers   → bench-ext4-*.json
```

Knobs: `DENSITY`, `SAMPLES`, `PHASES`, `IMG_SIZE`, `KEEP=1` (keep the loopback for repeat runs). Add `volumewake` to `PHASES` for the volume-app snapshot-wake number (the daemon runs with a `--volume-dir`). To drive an existing daemon by hand instead, point `crucible-bench --addr … --reflink-path <daemon work-base>` and add `--phases proxywake --proxy-addr … --proxy-domain …` for the wake number. `crucible-bench --help` lists every knob.

The end-to-end serverless story — a **postgres** on a volume, snapshot-wake vs cold-boot, timed from the wake trigger to a query answering — is a separate one-command run: `sudo … scripts/bench_serverless.sh` (boots its own daemon, needs `psql` on the host).

## Mass wake (the herd)

Scale-to-zero packs more sleeping apps than host RAM, so "everything wakes at once" is a designed-for scenario. `sudo … N=20 MEM=256 scripts/bench_masswake.sh` boots N scale-to-zero apps, drains them with `app sleep --all`, then fires N **concurrent** wakes and reports the wake-latency distribution (p50/p90/p99/max, both client end-to-end and daemon-measured), how many wakes the `--wake-min-free-mib` floor deferred to a clean `503` + retry (graceful degradation, not failure), and the host MemAvailable low-water mark. It only fails if an app never serves even after a sequential retry. Knobs: `N`, `MEM`, `WAKE_MIN_FREE`, `KEEP=1`.
