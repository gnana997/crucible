---
title: Private registries
description: "Pull private/authenticated images: store per-registry credentials on the daemon so run, app create, and app self-heal can pull them."
---

# Private registries

By default crucible pulls images anonymously, so a private image is unreachable.
`crucible registry login` stores a per-registry credential **on the daemon**,
which it then feeds to every image pull — `run`, `sandbox create --image`,
`app create/update`, and (critically) an app's **re-pull on restart or reboot**.

Because the credential lives on the daemon, a durable app on a private image
**survives a daemon restart**: the reconciler re-pulls with the stored
credential, no human in the loop. This is the case the local-docker workaround
(`docker login` + `docker pull`, then boot the local copy) can't cover — a
headless or remote daemon re-pulling on its own has no docker to lean on.

```bash
echo "$TOKEN" | crucible registry login ghcr.io -u my-user --password-stdin
crucible run ghcr.io/my-user/private-app:latest        # now authenticates
crucible app create web --image ghcr.io/my-user/web:1  # survives a daemon restart
```

## Commands

| Command | What |
|---|---|
| `registry login <host> [-u user] [--password-stdin \| -p secret]` | store a credential; prompts (masked) if no secret is given |
| `registry ls` | list stored credentials — **host + username only, never the secret** |
| `registry logout <host>` | remove a credential |

The secret is sent to the daemon and **never printed back** by any command or
endpoint. Prefer `--password-stdin` (scriptable, keeps the secret out of your
shell history); `--password` warns for that reason.

## What it covers

crucible supplies a static per-registry `(username, secret)` to
go-containerregistry, which handles the registry auth handshake for every OCI
registry. That single credential shape covers essentially every registry a
self-hosted user hits:

| Registry | Credential |
|---|---|
| **Docker Hub** (`docker.io`) | username + password or a **PAT** |
| **GHCR** (`ghcr.io`) | GitHub username + a **PAT** (`read:packages`) |
| **GitLab** (`registry.gitlab.com`) | username + deploy/PAT/CI-job token |
| **Quay** (`quay.io`) | username + password or a **robot token** |
| **Self-hosted** (Harbor, Nexus, Artifactory, `distribution`) | username + password/robot |
| **GCP Artifact Registry / GCR** | `_json_key` + the service-account key **JSON** as the secret |
| **Azure ACR** | a service principal (appId + secret), or the admin user + password |

Docker Hub's aliases (`docker.io`, `index.docker.io`, `registry-1.docker.io`)
canonicalize to one entry, so `registry login docker.io` matches a `library/…`
pull. A registry crucible has no credential for pulls **anonymously**, so public
images keep working with the store enabled.

### AWS ECR

ECR is the one registry a static credential can't fully serve: its credential is
a **12-hour token** minted from your AWS credentials, not a fixed secret. Log in
with that token and re-run periodically:

```bash
aws ecr get-login-password --region us-east-1 \
  | crucible registry login <acct>.dkr.ecr.us-east-1.amazonaws.com -u AWS --password-stdin
```

Native ECR auto-refresh (mint the token from an AWS role on each pull) is planned
for a later release.

## Storage & security

- Credentials live in a JSON file on the daemon (`--registry-store`, default
  `/var/lib/crucible/registry.json`; empty disables the feature, and pulls stay
  anonymous). The installer enables it by default.
- The file is written **`0600`**. Unlike API keys — which are stored hashed
  because they're only *verified* — a registry secret must be *replayed* to the
  registry, so it is stored **usable and NOT encrypted at rest**, the same
  posture as `~/.docker/config.json`. Protect the daemon host accordingly.
- crucible **never reads your `~/.docker/config.json`** and never runs
  credential-helper binaries — a deliberate choice so a host login can't leak
  into the root daemon, and so no helper binary runs as root. Credentials come
  only from `registry login`.
- Managing credentials is gated by the **`registry`** scoped-token operation
  (see [policy.md](policy.md)): a remote client needs a token that carries
  `registry` to add or remove a credential (listing needs only `read`). The
  secret rides the same authenticated (and, off loopback, TLS-required) channel
  as every other write. A scoped MCP agent has **no** registry tool — credential
  management is an operator action, not an agent one.
- **Pull credentials are daemon-global.** Stored credentials belong to the
  daemon, so *any* token that can create a sandbox or pull an image (the
  `create` operation) can pull a private image the daemon has a credential for,
  by referencing it — it does **not** need the `registry` operation. The secret
  itself never leaves the daemon, but on a shared daemon, treat "can create
  sandboxes" as "can use every stored private-registry credential." Mint separate
  daemons (or don't hand out `create`) when that isn't acceptable.
