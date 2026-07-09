# Releasing crucible

Maintainer checklist for cutting a release. Follow it in order — **step 2 is the
one that bites.**

## 1. Gate

```bash
make fmt vet lint
make race                # go test -race across the tree
make build               # daemon + CLI (embeds the guest agent)
```

## 2. Rebuild the guest-agent artifacts ⚠️

The guest agent lives in two places, and they refresh **differently**:

| Artifact | How it gets the agent | Refreshed by |
|---|---|---|
| **OCI images** (`crucible run <image>`) | the daemon **injects** the embedded agent when it converts the image | `make build` (embeds `bin/crucible-agent`) |
| **Profile rootfs** (`--profile`, `assets/**.ext4`) | the agent is **baked in at rootfs-build time** | `make rootfs` / `make profile` |

So `make build` alone is **not enough**. A profile `.ext4` built before an agent
change silently lacks the newer agent features (supervised services, the
interactive shell, …) and fails at runtime with:

> `guest agent does not support … — rebuild the rootfs with the current crucible-agent`

Rebuild **every shipped rootfs artifact** with the release's agent:

```bash
make agent                                     # static linux/amd64 agent
make rootfs BASE_ROOTFS=assets/ubuntu.squashfs \
            OUT_ROOTFS=assets/rootfs-with-agent.ext4

# every profile in profiles/profiles.env
make profile PROFILE=base
make profile PROFILE=python-3.12
make profile PROFILE=python-3.13
make profile PROFILE=node-20
make profile PROFILE=node-22
make profile PROFILE=node-24
make profile PROFILE=go-1.23
make profile PROFILE=go-1.24
```

Related gotcha: converted OCI images are cached by the **source image digest**,
so the agent version is *not* part of the cache key. After an agent change, a
cached image keeps its old agent until re-converted — `crucible image rm <ref>`
(or a fresh `--image-dir`) forces the re-convert.

## 3. Docs

- **`CHANGELOG.md`** — a new `[X.Y.Z]` entry (Added / Changed / Fixed / Security) **and** the compare-link footer.
- **`README.md`** — the status badge and the roadmap list (mark the new version *current*).
- **`docs/ROADMAP.md`** — move the delivered items into **Shipped**, mark the version *(current)*, and **prune anything this release actually shipped out of Planned** (stale "planned" items that already exist are the most common docs bug).
- **`docs/VISION.md`** — fix any claim that says a now-shipped feature is "planned."
- **`SECURITY.md`** — restate the supported scope if the threat model moved.

## 4. Verify

The smokes spin up their own daemon and share the chroot-base, so stop the
systemd daemon first:

```bash
sudo systemctl stop crucible
sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
     JAILER_BIN=/usr/local/bin/jailer \
     KERNEL=/var/lib/crucible/vmlinux \
     ROOTFS=./assets/rootfs-with-agent.ext4 \
     scripts/smoke_all.sh
```

Then install the built binary and prove the **installed** path works — this is
the gate that answers *"will a user who installs this hit a wall?"*:

```bash
sudo install -m0755 ./crucible /usr/local/bin/crucible
sudo systemctl start crucible
scripts/smoke_installed.sh        # unprivileged; must be green before tagging
```

If the installed daemon is missing feature flags (`--image-dir`, `--log-dir`),
image boot and durable logs won't work — check `/etc/crucible/crucible.env`.
A fresh `install.sh` writes them; upgraded hosts may need them added by hand.

## 5. Tag and publish

The version string comes from `git describe`, so the tag *is* the version.

```bash
git tag -a vX.Y.Z \
  -m "crucible vX.Y.Z — <one-line theme>" \
  -m "<one-line description>"
git push origin vX.Y.Z
```

Attach to the GitHub release: the `crucible` binary **and every profile `.ext4`
rebuilt in step 2** — the README quick-start downloads them by name, so a stale
uploaded profile ships a stale agent to every new user.
