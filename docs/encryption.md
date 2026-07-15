---
title: Volume encryption
description: "Encryption at rest for persistent volumes: each volume is its own LUKS container with its own key, so a tenant's data can be crypto-shredded by destroying that one key."
---

# Volume encryption

A [persistent volume](volumes.md) holds a stateful workload's real data — a
Postgres cluster, a Redis dump, a user's files. That data lives in a backing file
on the host disk. **Volume encryption** makes each volume its own LUKS2 container
with its **own key**, so the backing file is ciphertext at rest and a tenant's
data can be **crypto-shredded** by destroying that one key.

This is the standard **encryption-at-rest** model (the same one AWS EBS gives
you): it protects a **stolen, seized, RMA'd, or decommissioned disk**, and it
gives you a key you control and can revoke. See [What it protects](#what-it-protects-and-what-it-doesnt)
for the honest boundary — it is **not** protection against a compromised host.

## Enable it

Encryption is **opt-in**: the daemon needs a master key. Point it at a key file
(generated on first use) or supply the key in the environment:

```bash
crucible daemon … --volume-dir /var/lib/crucible/volumes \
                  --volume-encrypt-key-file /var/lib/crucible/volume.key

# or, key from a KMS / systemd credential (takes precedence over the file):
CRUCIBLE_VOLUME_KEY="$(cat key.b64)" crucible daemon … --volume-dir …
```

Add `--volume-encrypt` to make **every new volume encrypted by default**; without
it, encryption is available but a volume is only encrypted when you ask
(`--encrypt`). With **no key configured**, encryption is off and `--encrypt` /
`volume shred` are rejected — there is no silent fallback.

> ⚠️ **Back up the master key.** It is the only thing that can open your encrypted
> volumes — losing it means losing **all** of their data. The key lives outside
> the volume store (a key file, `CRUCIBLE_VOLUME_KEY`, or a KMS), so a stolen disk
> alone is inert. Keep it somewhere safe and separate, and treat it like the
> database it unlocks.

The volume key is independent of the [secrets](secrets.md) master key — point both
flags at the same file if you want one key, or keep them separate.

## Create an encrypted volume

```bash
# Explicitly encrypted, regardless of the daemon default:
crucible volume create pgdata --encrypt --size 10G

# Explicitly plaintext, even when --volume-encrypt is the daemon default:
crucible volume create cache --no-encrypt

# `volume ls` shows which volumes are encrypted:
crucible volume ls
# NAME     SIZE   ENCRYPTED  ATTACHED  HOST   AGE
# pgdata   10G    yes        -         host1  2m
# cache    2G     no         -         host1  1m
```

Attach it to an app or sandbox exactly like any volume — the encryption is
transparent to the guest:

```bash
crucible app create db --image postgres:16-alpine \
  --volume pgdata:/var/lib/postgresql/data \
  --env POSTGRES_PASSWORD=… --env PGDATA=/var/lib/postgresql/data/pgdata
```

## How it works

Each encrypted volume is a **LUKS2 container** (`aes-xts-plain64`, a 512-bit key —
AES-256-XTS, the disk-encryption standard) over its backing file. A fresh random
**per-volume key** unlocks it; that key is sealed under the daemon master key
(AES-256-GCM, bound to the volume name) and stored in the volume record — **never
in the clear on disk**. The record's wrapped key is the *only* copy.

When the volume attaches, the daemon opens the LUKS container to a decrypted
device (`/dev/mapper/crucible-vol-<name>`) and — under the jailer — stages **that
device node** into the VM's chroot (the same mechanism the jailer uses for
`/dev/kvm`), never the ciphertext file. The guest sees an ordinary block device
and mounts plaintext; the bytes on disk are ciphertext. Encryption and decryption
happen in the kernel's device-mapper layer, so it is **transparent** — including
to the snapshot/wake memory pager, which pays no per-page cost.

Because the guest key is a high-entropy random value, the LUKS keyslot uses a fast
KDF: opening a volume (on attach, and on every [scale-to-zero](serverless.md) wake)
stays sub-second.

### Encrypted at rest, including while asleep

A [scale-to-zero](serverless.md) app snapshots and stops its VM when idle. For an
encrypted volume, the daemon **closes the decrypted device on sleep** (the
single-writer claim is kept) and **re-opens it on wake** — so a slept database's
data is ciphertext at rest for the entire sleep, not left decrypted on the host.

## Backups

A [backup](backups.md) of an encrypted volume is the **ciphertext** container, and
the backup record carries the volume's wrapped key — so it is inert without the
master key, and a `restore` re-wraps that key under the new volume's name and
brings the data back:

```bash
crucible app sleep db                 # quiesce (or detach) first
crucible volume backup pgdata
crucible volume restore --from <backup-id> --to pgdata-copy   # also encrypted
```

`volume clone` of an encrypted volume is **not yet supported** (an independent
clone needs a fresh key — a full re-encrypt); restore a backup instead.

## Crypto-shred

Deleting an encrypted volume's key makes its data **permanently unrecoverable**
without touching the ciphertext blocks — instant, and provable:

```bash
crucible volume shred pgdata     # destroys the keyslots + deletes the wrapped key
```

`shred` is refused while the volume is attached to a live sandbox, and on a
plaintext volume (use `volume rm`). It is gated by the default-deny `delete`
scoped-token op. For a managed control plane, a per-tenant key means deleting a
tenant crypto-shreds all their volumes at once.

## What it protects (and what it doesn't)

| Threat | Protected? |
|---|---|
| Stolen / seized / RMA'd / decommissioned disk | ✅ ciphertext at rest |
| Crypto-shred a volume (or a whole tenant's key) | ✅ data unrecoverable, immediately |
| One guest reading another's volume | ✅ single-writer + separate keys + isolation |
| A compromised **host root** | ❌ it holds the live device-mapper (the AWS-EBS model) |
| The guest itself | ❌ it legitimately mounts plaintext |

Closing the host-root gap needs confidential computing (encrypted guest RAM),
which Firecracker does not support and which is fundamentally incompatible with
the lazy-paging that powers sub-second [scale-to-zero](serverless.md) wake. So the
honest claim is **"encryption at rest with per-tenant keys and crypto-shred,"**
never "we can't read your data."

## Slept apps and the memory snapshot

Volume encryption covers a workload's **data** on disk. There is one more surface
to understand for a **slept** [scale-to-zero](serverless.md) app, and it's worth
being precise about because it's where crucible differs from a stateless
serverless-database design.

When crucible sleeps an app it **snapshots the whole VM, including its RAM**, to a
memory file under `--work-base`, so it can **wake warm in ~170 ms** — buffers hot,
no reconnect, no cold query. That snapshot can contain cached rows (a database's
`shared_buffers`), so **the memory file is at-rest data too**. A stateless design
instead discards RAM on suspend and reconnects to storage on wake — no memory
file, but a **cold** wake that re-warms its buffers on the first queries.
crucible trades that one extra surface for a much faster wake.

So there are two honest ways to keep a slept database encrypted at rest, and you
choose per the tradeoff you want:

- **Warm (default):** keep snapshot-wake, and put `--work-base` on an **encrypted
  filesystem** (LUKS / dm-crypt). Because device-mapper encryption is transparent
  — the kernel decrypts pages as the snapshot pager faults them in — this adds
  **no per-page wake cost**, and every byte the daemon writes (the memory file,
  the writable rootfs, the volume containers) is ciphertext at rest. This is the
  recommended setup.
- **Cold:** a stop/start sleep that never writes RAM to disk — zero
  memory-at-rest surface, at the cost of a cold wake.

> ℹ️ The daemon does **not** encrypt the memory file itself: doing so would force
> the wake path to decrypt every faulted page, taxing exactly the sub-second wake
> that scale-to-zero depends on. An encrypted `--work-base` gets the same result
> for free — the kernel does the crypto below the pager.

### Startup advisory

When volume encryption is enabled but the daemon can positively see that
`--work-base` sits on **unencrypted** storage, it logs a warning at startup:

```
volume encryption is on but --work-base is on unencrypted storage: a slept app's
memory snapshot (cached rows, buffers) is written to disk in the clear. Put
--work-base on a dm-crypt/LUKS filesystem for full encryption at rest.
```

It stays silent when `--work-base` is on an encrypted or ephemeral (tmpfs)
filesystem, or when the backing storage can't be classified — so a warning means
there is really something to fix.
