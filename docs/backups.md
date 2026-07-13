---
title: Volume backups
description: "Point-in-time, consistent backups of a volume, restorable to a new volume. Back up a detached, slept, or live (fsfreeze) volume; clone a volume; keep backups on your own storage."
---

# Volume backups

A [volume](volumes.md) keeps a database's data safe across a sandbox's life, a
hard kill, a redeploy, sleep, and a daemon restart. A **backup** goes one step
further: a point-in-time, consistent copy of a volume that you can **restore into
a new volume** at any time. The same copy primitive also **clones** a volume, so
you can fork a database for a preview or test environment.

```
crucible volume backup db                          # back up the "db" volume
crucible volume backup ls db                        # list its backups
crucible volume restore --from <id> --to db-copy    # restore into a NEW volume
crucible volume clone db db-preview                 # copy a volume directly
```

## Consistency by state

A backup is only as good as the filesystem image it copies, so crucible picks the
safe method automatically from the volume's current state:

| Volume state | How it is backed up | Consistency |
|---|---|---|
| **Detached** (no sandbox attached) | copy the backing file directly | filesystem-consistent (nothing is writing) |
| **Slept** (a scale-to-zero app that is asleep) | copy the quiescent, already-fsync'd backing file | filesystem-consistent |
| **Live** (attached to a running sandbox) | **freeze the guest filesystem, copy, thaw** | filesystem-consistent |

For a live volume, crucible tells the guest agent to `FIFREEZE` the volume's
filesystem (only that mount, never the root filesystem the agent runs from),
copies the backing file while writes are held, then `FITHAW`s it. The freeze
lasts only as long as the copy, and the agent auto-thaws on a watchdog if the
daemon ever fails to, so a running database is backed up with no downtime and no
risk of a stuck freeze. Restore and clone follow the same rules: a live source is
frozen for the copy.

## Live backups need a reflink filesystem

A live backup freezes the guest only for the duration of the copy, so that copy
must be **O(1)**. On a reflink filesystem (**btrfs** or **XFS**) it is an instant
copy-on-write clone. On **ext4** there is no reflink, so a copy is a full
byte-for-byte read that could hold the freeze for seconds. crucible therefore
**refuses a live backup when the backup filesystem is not reflink-capable** and
asks you to sleep the app first (a slept or detached backup works on any
filesystem). Put the backup directory on btrfs or XFS to back up live databases.

## Where backups live

Backups are written under `--backup-dir` (default `<volume-dir>/backups`). Point
it at a different disk, or at a mounted network or object store, for durability
that survives the loss of the volume's own disk:

```
crucible daemon --volume-dir /var/lib/crucible/volumes \
                --backup-dir  /mnt/backups
```

Two trade-offs to know:

- A backup directory on the **same filesystem** as the volumes reflinks (O(1),
  cheap), but a same-disk backup does not survive that disk dying. It protects
  against an accidental `rm`, a bad migration, or application-level corruption you
  want to roll back from.
- A backup directory on **another disk or host** survives disk death and lets you
  move a volume to another machine, but the copy is a full byte copy (reflink
  cannot cross filesystems), and a live backup there is refused (not
  reflink-capable).

Native off-host targets (S3, rsync) and incremental backups are planned for a
later release; today a `--backup-dir` on a mounted volume is the way to keep
backups off-host.

## Restore and clone

- `volume restore --from <backup-id> --to <name>` materializes a backup into a
  **new** volume. It never overwrites an existing volume, so a restore can never
  clobber live data. Attach the new volume to a sandbox or app as usual.
- `volume clone <src> <dst>` copies a volume straight into a new one, with no
  backup record in between. Use it to fork a database for a preview environment or
  a test run. The clone is fully independent: writing to it does not touch the
  source.

A restored or cloned volume mounts read-write in a guest and, if its source was
not cleanly unmounted, replays its journal on first mount, exactly like any
crash-recovered filesystem.

## Command reference

| Command | What it does |
|---|---|
| `volume backup <name>` | Back up a volume; prints the backup id |
| `volume backup ls [<name>]` | List backups (all, or for one volume) |
| `volume backup rm <id>...` | Delete backups by id |
| `volume restore --from <id> --to <name>` | Restore a backup into a new volume |
| `volume clone <src> <dst>` | Copy a volume into a new one |

All are available over the REST API (`POST /volumes/{name}/backups`, `GET
/backups`, `POST /volumes/{name}/restore`, `POST /volumes/{name}/clone`) and as
MCP tools (`volume_backup`, `volume_restore`).

Acceptance: `scripts/smoke_backups.sh` covers a detached backup (loop-mounted to
verify the data), a slept-app backup, a live backup via fsfreeze on a reflink
filesystem, a restore into a new volume, and an independent clone. See also
[volumes.md](volumes.md) and [serverless.md](serverless.md).
