---
title: Secrets
description: "Encrypted secret bundles: store sensitive config once, inject it into an app's environment — without the value ever touching the app spec, backups, or logs."
---

# Secrets

`--env` puts config in the app spec — which is stored **in cleartext** in the
app database and rides into every `admin backup`. That's fine for a log level;
it's wrong for a database password. **Secret bundles** fix that: sensitive values
live in a dedicated store, **encrypted at rest**, and reach the guest as
environment variables **without the value ever appearing in the app spec, the
API, backups, or logs**.

A secret is a **bundle**: a named set of `key → value` pairs (like a Kubernetes
Secret). An app injects a bundle with **envFrom** — every key becomes an
environment variable.

## Enable it

Secrets are **opt-in**: the daemon needs a master key that encrypts the store.
Point it at a key file (generated on first use) or supply the key in the
environment:

```bash
crucible daemon … --secrets-key-file /var/lib/crucible/secrets.key
# or, key from a KMS / systemd credential (takes precedence over the file):
CRUCIBLE_SECRETS_KEY="$(cat key.b64)" crucible daemon …
```

With **no key configured, secrets are disabled** and the `/secrets` routes answer
501 — there is no silent plaintext fallback. Managing secrets is gated by the
default-deny `secret` scoped-token op.

> ⚠️ **Back up the master key.** Losing it means losing every secret — the store
> is useless without it. The key is deliberately **excluded** from `admin backup`
> (so the backup alone is inert), so keep it somewhere safe and separate.

## Create a bundle

The most common path is a `.env` file — the whole file becomes one bundle:

```bash
crucible secret set web-env --from-env-file .env
```

Or set one key at a time (the value comes from stdin or `--from-file`, **never**
the command line):

```bash
printf '%s' "$DB_PASSWORD" | crucible secret set web-env DATABASE_URL
crucible secret set web-env API_KEY --from-file api-key.txt
```

Inspect without ever seeing values:

```bash
crucible secret ls              # bundle names
crucible secret ls web-env      # that bundle's KEY names (not values)
crucible secret rm web-env
```

## Inject into an app (envFrom)

```bash
# inject every key of one or more bundles as env vars
crucible app create web --image … --secrets web-env

# one-shot: import a .env into a bundle named <app>-env AND inject it
crucible app create web --image … --secrets-from .env
```

The app spec stores only the **bundle name** in `secret_env_from` — so `app get`
and backups carry no secret material. At boot the daemon decrypts each bundle and
merges its keys into the environment, with precedence **image `ENV` → secret
bundles → `--env`** (last wins), so a plaintext `--env` can still override.

A bundle referenced by an app must exist (create/update fails fast otherwise); a
bundle deleted out from under a running app is skipped on the next boot with a
warning.

## What's protected — and what isn't

| Surface | Secret value exposed? |
|---|---|
| the app spec / `app get` | **no** — only the bundle name |
| `admin backup` | **no** — the store rides it as ciphertext; the key is not in the backup |
| the `/secrets` API and `crucible secret ls` | **no** — names and key names only |
| daemon logs | **no** — the resolved environment is never logged |
| the on-disk secret store | **no** — AES-256-GCM sealed |
| the **running guest's memory** (and its snapshot) | **yes** — see below |

### The honest limit: snapshot residency

Once injected, a secret is in the guest process's environment — in **guest RAM**.
Because crucible snapshots guest RAM to disk for sleep / wake / fork, that
plaintext is written to the snapshot's memory file under `--work-base`. So a
running or slept app's secrets are **guest-RAM-grade**: recoverable by host-root
or by stealing an unencrypted disk — the same posture as any VM platform (a
snapshot is just persisted RAM).

What this release changes is the *at-rest config* exposure, which was far worse:
the store, backups, and API no longer leak. For the runtime residual:

- **Put `--work-base` on an encrypted filesystem** (LUKS/dm-crypt) so disk theft
  doesn't yield snapshot memory.
- Encrypting the snapshot memory file itself is planned (a later release), which
  closes disk-at-rest for snapshots too.

### Rotation

A running or slept instance holds the old value in its RAM. After changing a
secret, **redeploy** the app (`app update`) to re-inject the new value.

## Non-sensitive config

Keep using `--env` for values that aren't sensitive (a port, a log level) — it's
simpler, and plaintext-in-the-spec is fine for those. Move only the secrets to a
bundle.
