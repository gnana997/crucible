---
title: Snapshot & fork
description: "Freeze a running server, then fork it into independent copies that diverge without touching each other — crucible's core primitive, in one take."
---

# Snapshot & fork: the core primitive

crucible's headline trick is that a **running** program can be frozen and then
forked into many independent copies in milliseconds. Fork a warm server after the
expensive setup is done — dependencies installed, caches hot, a request already
served — and each child resumes from that exact state instead of repeating it.

![crucible: run nginx in a microVM, push content in, snapshot the running server, fork it onto its own port, and watch the two VMs serve different answers](../demo/fork.gif)

The take above is one unbroken sequence:

1. **Boot** `nginx:alpine` in its own microVM with a published port.
2. **Push content in** with `crucible cp` — no image rebuild.
3. **Snapshot the running server** — memory, page cache, and the writable rootfs,
   all frozen mid-flight.
4. **Fork it** onto its own port. The child is a live copy of the running VM.
5. **Diverge the fork** — change only the copy's content. The two VMs now serve
   different answers, and neither can see or corrupt the other.

## Why it's independent, not just a copy

A naïve VM clone shares the parent's random-number state and machine identity —
two "copies" that generate the same TLS keys, the same UUIDs, the same
`/etc/machine-id`. crucible forks are **clone-safe**: every child gets its own
network namespace and, before it is ever reachable, its RNG is reseeded and its
machine identity rotated. A fork is a genuinely separate machine that happens to
start from a shared past, not a photocopy.

That safety is applied *fatal-before-reachable* — if the reseed or identity
rotation fails, the fork fails rather than coming up with duplicated entropy.

## Why it's fast

Forks resume with **lazy memory** (`userfaultfd`): a child pages in only the
working set it actually touches, so its cost is O(pages used), not O(guest RAM),
and the host never copies the whole memory image per fork. On the reference box a
fork is **~9× faster than a cold boot**, and hundreds of forks run concurrently,
bounded by RAM rather than by copying — see [benchmarks.md](benchmarks.md).

## Do it yourself

```bash
SBX=$(crucible run nginx:alpine -p 8080:80)
crucible cp ./index.html $SBX:/usr/share/nginx/html
SNP=$(crucible snapshot create $SBX)
crucible fork $SNP --count 5            # five warm, independent copies
crucible fork $SNP -p 8081:80           # or one, on its own port
```

The full command reference — `snapshot create/ls/inspect/rm` and `fork` with its
flags — is in [cli/snapshots.md](cli/snapshots.md). MCP agents get the same via
the `snapshot` and `fork` tools ([mcp.md](mcp.md)).

> Forking is the primitive; **[apps](apps.md)** are the durable layer on top, and
> **[scale to zero](apps.md#scale-to-zero)** re-points this same snapshot/restore
> machinery at idle apps — sleep is a snapshot you never resume, wake is a restore
> in place.
