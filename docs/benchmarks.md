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
| Fork (warm → child), p50 | 690 ms | **207 ms** — 3.3× faster |
| Fork fan-out @ 64-way | 267 ms/child · 3.7/s | **22 ms/child · 45/s** — ~12× |
| Snapshot, p50 | 738 ms | **425 ms** |
| 128 forks: wall time | 34.8 s | **2.1 s** |
| 128 forks: host RAM | 4.9 GiB | **1.2 GiB** |

**If you fork a lot, put `--work-base` on btrfs or XFS.** The rest of this page reports both, so you can see the floor (ext4) and the ceiling (reflink).

## Setup

| | |
|---|---|
| CPU | AMD Ryzen AI 9 HX 370 (24 threads) |
| Kernel | 7.0 · Free RAM ~13–15 GiB (other workloads paused) |
| Sandbox | 1 vCPU / 512 MiB, default (`python-3.12`, 1 GiB) rootfs, no network |
| Daemon | v0.2.0 under jailer, `CRUCIBLE_MAX_FORK=128` |
| Samples | 50 per latency op (5 warmup discarded) |

## Latency

| Operation | ext4 p50 | btrfs p50 | p90 / p99 (btrfs) |
|---|---|---|---|
| Exec roundtrip (`true`, warm) | 2.7 ms | **2.7 ms** | 2.9 / 3.3 ms |
| Fork (warm snapshot → child) | 690 ms | **207 ms** | 221 / 232 ms |
| Snapshot (running → on disk) | 738 ms | **425 ms** | (tail to ~2.5 s) |
| Cold create (boot → agent ready) | 2.00 s | 1.87 s | 1.94 / 1.99 s |

**Fork is ~9× faster than a cold create even on ext4, and ~3× faster again on reflink.** That's the point of snapshot/fork: pay the ~2 s boot + setup once, then branch cheaply. Exec overhead is ~3 ms (a vsock roundtrip) and is filesystem-independent. Snapshot's btrfs p50 is ~0.4 s with occasional multi-second tails from writing the 512 MiB memory image through writeback; the median is representative.

## Fork fan-out

Forking *N* children from one snapshot in a single call. Per-child cost falls with batch size as fixed per-fork overhead amortizes — and reflink pulls far ahead as N grows:

| Children | ext4 per-child | btrfs per-child | btrfs throughput |
|---|---|---|---|
| 1 | 909 ms | 296 ms | 3.4/s |
| 4 | 426 ms | 81 ms | 12.4/s |
| 16 | 303 ms | 38 ms | 26.6/s |
| 64 | 267 ms | 22 ms | 44.8/s |
| 128 | — | **16 ms** | **61/s** |

(On ext4 each child copies a full 1 GiB rootfs, so a 64-way fan-out moves ~64 GiB of disk — the throughput ceiling is the disk, not the runtime.)

## Memory efficiency

The lazy-`userfaultfd` payoff: guest RAM is served on demand from the snapshot's memory file, so forks share pages instead of each copying 512 MiB. Forking **128 children** from one warm snapshot:

| | ext4 | btrfs |
|---|---|---|
| Host RAM consumed | 4.9 GiB (~38 MiB/fork) | **1.2 GiB (~9.5 MiB/fork)** |
| vs naïve *128 × 512 MiB* = 64 GiB | **13× less** | **54× less** |

Both obliterate the naïve cost. reflink pulls ahead because ext4's per-fork rootfs copies add page-cache pressure that reflink avoids. The win also *grows* with fork count — the more children share a snapshot's pages, the lower the marginal cost per fork.

## Density (reflink)

Forking toward a live-sandbox target on btrfs and watching free RAM:

| Live sandboxes | Free RAM |
|---|---|
| 128 | 13.4 GiB |
| 256 | 12.3 GiB |
| 384 | 10.2 GiB |
| **512** | **7.4 GiB** |

**512 concurrent microVMs** on a laptop, with 7.4 GiB headroom left — 512 was the test's cap, not a ceiling. On reflink, density is RAM-bound (~14 MiB marginal per fork). On **ext4** it's *disk*-bound instead — each fork writes a full ~1 GiB rootfs — so plan disk accordingly, or use reflink.

## Reproduce

```bash
make bench                                   # builds ./bin/crucible-bench

# a daemon under jailer; raise the fork cap; point --work-base at your target FS:
CRUCIBLE_MAX_FORK=128 sudo ./crucible daemon \
  --firecracker-bin … --jailer-bin … --kernel … --rootfs … \
  --work-base /path/to/work --chroot-base /path/to/jailer

./bin/crucible-bench \
  --samples 50 --fanout 1,4,16,64,128 --mem-forks 128 --density 512 \
  --json bench-results.json
```

For the ext4 numbers, point `--work-base`/`--chroot-base` at an ext4 path; for the reflink numbers, at a btrfs or XFS (`reflink=1`) mount. `crucible-bench --help` lists every knob (sandbox size, phases, profile).
