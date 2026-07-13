---
title: Persistent volumes
description: "Durable block-device volumes that outlive a sandbox or app — data that survives destroy/re-create, hard kills, redeploys, sleep, and daemon restarts."
---

# Persistent volumes

By default a sandbox is ephemeral: its writable rootfs is a per-VM clone that
vanishes when the VM goes away. A **volume** is the opposite — a named,
fsync-honest block device that **outlives** the sandbox or app it attaches to.
Mount one at a database's data directory, a browser's profile, or an upload
folder, and the data survives destroy/re-create, a hard VM kill, an app
redeploy, sleep, and a daemon restart.

```bash
crucible volume create pgdata --size 5G
crucible run postgres:16 --volume pgdata:/var/lib/postgresql/data \
  --memory 2048 -e POSTGRES_PASSWORD=secret
```

Volumes require the daemon to have a volume directory: start it with
`--volume-dir <dir>` (see [Storage & the daemon](#storage--the-daemon)).

## The one rule: single-writer

A volume is backed by an **ext4** filesystem, which allows exactly one writer.
Everything else about volumes follows from that:

- A volume may be attached to **at most one live sandbox** at a time. A second
  concurrent attach is refused.
- A volume-backed **app cannot scale out** (`--max-scale`/`--min-scale > 1`) —
  N replicas would be N writers.
- A volume-backed app **redeploys via destroy-then-boot** (a brief blip), not the
  zero-downtime flip, because the flip runs two instances at once.
- A volume-backed app **snapshot-sleeps and wakes in place** (~170 ms on reflink),
  just like a stateless app: sleep snapshots the instance and stops the VMM (RAM
  freed, the volume host-fsync'd), wake restores it with the volume re-attached —
  no cold boot, no DB recovery (v0.6.2).

This is the honest trade: **stateless stays magic; stateful gets durable.**

## Sandbox volumes

Attach a volume to any sandbox with `--volume NAME:/absolute/path` (repeatable).
The volume is created and formatted on first use, then reattached by name:

```bash
# write into a fresh volume
SBX=$(crucible run alpine --volume work:/data)
crucible sandbox exec $SBX -- sh -c 'echo hello > /data/file'
crucible rm $SBX                       # the sandbox is gone…

# …the volume persists — re-attach and the data is still there
SBX=$(crucible run alpine --volume work:/data)
crucible sandbox exec $SBX -- cat /data/file   # → hello
```

`--volume` works on `crucible run` and `crucible sandbox create`.

## Managing volumes

```bash
crucible volume create <name> [--size 5G]   # explicit create + size
crucible volume ls                           # NAME  SIZE  ATTACHED  HOST  AGE
crucible volume rm <name>                    # delete the volume and its data
```

- `create` pre-provisions at an explicit size. Without it, `run --volume` still
  auto-creates a volume on first use at the daemon's `--volume-default-size`
  (docker-`-v` ergonomics) — but an explicit `create --size` is honored by a
  later `run --volume` (it uses the recorded size).
- `rm` is **refused while the volume is attached** to a live sandbox; remove or
  stop the sandbox first. It deletes the backing file, so the data is gone.
- Records are durable: `volume ls` survives a daemon restart.

The same operations are available over the REST API (`/volumes`) and to agents
via the MCP tools `volume_create`, `list_volumes`, `delete_volume`.

## App volumes

Give a durable **app** a volume with `app create --volume NAME:/path`. The data
survives redeploys, sleep, and daemon restarts (the daemon re-creates the app
from spec and re-attaches the volume):

```bash
crucible app create db --image postgres:16 \
  --volume pgdata:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD=secret --port 5432
```

Because a volume app is single-writer, its lifecycle differs from a stateless
app:

| | Stateless app | **Volume app** |
|---|---|---|
| Redeploy (`app update`) | zero-downtime flip | **destroy-then-boot** (brief blip) |
| Sleep / wake | snapshot, ~125 ms wake | **snapshot**, ~170 ms wake (reflink) |
| Scale out (`--max-scale`) | yes | **no** (single writer) |
| Idle-sleep without `--port` | n/a | **rejected** (needs the proxy to wake) |

An HTTP volume app (`--port`) can still **scale to zero and wake on request** —
the wake is a cold-create instead of a snapshot restore. A TCP-only volume app
(no `--port`) can't idle-sleep yet (wake-on-connection arrives in a later
release), so it runs always-on.

## Durability guarantees

- **fsync is honest.** Volume drives attach to Firecracker with
  `cache_type=Writeback`, so a guest `fsync` becomes a host `fdatasync` on the
  backing file. Committed data survives a **hard kill** of the VM (verified by
  `scripts/smoke_volumes.sh`).
- **Sleep flushes first.** Before a volume app's instance is stopped, the guest
  is `sync`'d so un-fsync'd writes reach the volume.
- **Crash-consistent, then clean.** An unclean stop leaves an ext4 journal that
  recovers on the next mount; a database's own WAL layers on top of that.

The floor is the host filesystem under `--volume-dir`: crucible can't be more
durable than the disk you point it at.

## Storage & the daemon

- **`--volume-dir <dir>`** enables volumes and holds the backing files
  (`<name>.img`) plus a small bbolt index. It **must be on the same filesystem
  as `--chroot-base`** — volumes hardlink into the jail (a cross-filesystem
  volume dir is rejected rather than silently copied, which would break
  persistence). Put it on the disk you trust.
- **`--volume-default-size`** (default 2 GiB) sizes a volume created implicitly
  by `run --volume` with no prior `volume create`.
- The daemon host needs **`mkfs.ext4`** (`e2fsprogs`) — preflighted at startup.

## What's next

- **Serverless over TCP** *(shipped v0.6.1)* — a wake-on-connection forwarder wakes
  a scaled-to-zero postgres (or any TCP service) on the incoming connection, not
  just an HTTP request, so a volume app can be TCP-only *and* idle-sleeping. See
  [serverless.md](serverless.md).
- **Instant wake** *(shipped v0.6.2)* — a volume app snapshot-sleeps and wakes in
  place in ~170 ms (reflink), no cold boot or DB recovery. See
  [benchmarks.md](benchmarks.md).
- **Volume backups** *(shipped v0.6.3)* — a consistent point-in-time copy of a
  volume for backup, clone, and off-host durability, including a no-downtime live
  backup via fsfreeze. See [backups.md](backups.md).

See the [ROADMAP](ROADMAP.md) for sequencing.
