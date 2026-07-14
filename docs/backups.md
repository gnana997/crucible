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

## No downtime, measured

A live backup pauses a database only for the freeze, and only briefly. Measured on
`postgres:16-alpine` on a btrfs volume (1 vCPU / 512 MiB, AMD Ryzen AI 9 HX 370),
under **16 concurrent writers sustaining ~2,500 padded-row INSERTs per second**,
taking 12 live backups back to back:

| Metric | Value |
|---|---|
| Live backup operation (freeze + reflink copy + thaw) | **~90 ms** median (73 to 111 ms) |
| Typical INSERT latency | ~6 ms |
| Worst INSERT latency during a backup | ~235 ms |
| **Failed transactions** | **0** |

The whole backup, freeze included, takes about 90 ms, and it barely moves with load
because the copy is an O(1) reflink (pushing to 32 writers left it at ~105 ms). The
freeze itself is a slice of that: an fsync of the volume plus the reflink. No
transaction failed: a query that lands during the freeze waits for it and then
completes, so the client sees a brief latency blip, not an error. That worst-case
blip grows with write concurrency (more in-flight commits queue behind the freeze,
then flush together on thaw), but it stays a pause, not an outage. At 32 concurrent
writers the busiest query saw a few hundred milliseconds and still zero failures.

Reproduce with `scripts/bench_backup.sh` (`WRITERS=N SAMPLES=M`).

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

## Control-plane backup (`crucible admin backup`)

Volume backups protect your *data*; the control-plane backup protects the
daemon's *knowledge of everything else* — without it, a rebuilt host has
volumes but no apps, tokens, or pull credentials. One command captures it all,
hot, while the daemon keeps serving:

```bash
crucible admin backup                       # writes crucible-backup-<ts>.tar.gz
crucible admin backup -w cp.tar.gz          # explicit filename
crucible admin backup | ssh vault 'cat > crucible.tar.gz'   # piped = streamed
```

The archive holds up to five entries — each present only when its component is
configured:

| Entry | Store | How it's captured |
|---|---|---|
| `app.db` | durable app records (`--app-db`) | bbolt read transaction — consistent, hot |
| `tokens.json` | API keys + policies (`--token-file`) | read under the store lock (hashes, not usable keys) |
| `volume-index.db` | volume + backup records (`--volume-dir`) | bbolt read transaction |
| `registry-credentials.json` | registry logins (`--registry-store`) | read under the store lock — **usable secrets** |
| `manifest.json` | version, timestamp, hostname, entry list | written last |

Each store's copy is individually consistent; the archive as a whole is not a
cross-store transaction (the stores are independent, and skew between them is
harmless). **Volume data is deliberately not included** — pair this with
`crucible volume backup`, ideally with `--backup-dir` on off-host storage.

The endpoint (`GET /admin/backup`) is gated by the **default-deny
`admin_backup`** scoped-token op: the archive carries usable registry secrets
and the full token/policy state, so treat the file like a credential file
(it is written `0600`).

### Restore procedure

Restore is deliberately a documented procedure onto a **stopped** daemon, not a
command — laying store files under a live daemon invites corruption:

1. Stop the daemon (`systemctl stop crucible`).
2. Extract the archive and copy each entry to its configured path:
   `app.db` → `--app-db`, `tokens.json` → `--token-file`,
   `volume-index.db` → `<volume-dir>/index.db`,
   `registry-credentials.json` → `--registry-store`.
3. Restore volume *data* from your volume backups if this is a fresh host
   (`volume restore` needs the daemon up — do it after step 4).
4. Start the daemon.

On startup the reconciler treats the restored app records as desired state and
**re-creates every app that was running** — a restore is not just records
coming back, it is the apps healing themselves. Pre-disaster API keys work
immediately (the token store is the same file).

Acceptance: `scripts/smoke_cp_backup.sh` runs the full disaster: state created,
archive taken, every store deleted, archive restored, and the app observed
self-healing back to running with the old token still valid.
