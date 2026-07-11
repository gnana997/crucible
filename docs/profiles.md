---
title: Rootfs profiles
description: "Pre-baked guest rootfs images a sandbox can boot from (base, python-3.12, node-22), how they are built, and how the daemon discovers them."
---

# Rootfs profiles

A **profile** is a pre-baked guest rootfs a sandbox can boot from — `base`, or a language environment like `python-3.12` / `node-22` / `go-1.24`. Profiles are built from the **official language base images** (`python:3.12-slim`, `node:22-bookworm-slim`, …), so the toolchain lives exactly where the language expects it: `pip`, `npm`, `go`, `venv` all behave the way agent code assumes, with no bolted-on tarballs or PATH surprises.

## How a profile is built

`scripts/build-profile.sh` (wrapped by `make profile`) turns an official image into a Firecracker-bootable ext4:

1. **`docker build`** from the base image (`profiles/Dockerfile`), injecting only what a microVM guest needs on top of the native toolchain: an init (`systemd` + `systemd-networkd`), the `crucible-agent` systemd unit, and crucible's static DNS config.
2. **`docker export`** the container filesystem to a tar.
3. **`mkfs.ext4 -d`** the tree into an image, inside a single `fakeroot` session so `root:root` ownership survives without host-side sudo.

The container is never *run* — Firecracker boots the exported rootfs directly with `init=/sbin/init`.

## Building

Prerequisites: **Docker**, plus `fakeroot` and `e2fsprogs` (`mkfs.ext4`/`debugfs`). Then:

```bash
make agent                          # build the guest agent first
make profile PROFILE=python-3.12    # → assets/profiles/python-3.12.ext4
make profile PROFILE=node-22
make profile PROFILE=base
```

Images are **linux/amd64** (Firecracker requires KVM on x86-64). Each language image is a few hundred MB; they are **not** committed to the repo (`assets/` is gitignored).

## Available profiles

The set lives in [`profiles/profiles.env`](../profiles/profiles.env) as `<profile>  <base-image>`:

| Profile | Base image |
|---|---|
| `base` | `debian:12-slim` |
| `python-3.12`, `python-3.13` | `python:3.x-slim-bookworm` |
| `node-20`, `node-22`, `node-24` | `node:2x-bookworm-slim` |
| `go-1.23`, `go-1.24` | `golang:1.x-bookworm` |

**Adding a version** is one line in `profiles.env` (pin an exact base tag for reproducibility), then `make profile PROFILE=<name>`. The profile name is whatever a create request selects.

## Serving profiles

Point the daemon at a directory of built images:

```bash
crucible daemon \
  --firecracker-bin /usr/local/bin/firecracker \
  --kernel  assets/vmlinux \
  --rootfs  assets/profiles/base.ext4 \
  --rootfs-dir assets/profiles \
  --jailer-bin /usr/local/bin/jailer
```

The daemon scans `--rootfs-dir` at startup: each `<name>.ext4` becomes a profile named `<name>`. `--rootfs` is the default used when a request names no profile.

**Aliases** are just symlinks — `ln -s node-22.ext4 assets/profiles/node.ext4` gives a `node` profile pointing at the current LTS. A broken symlink fails at startup rather than at create time.

## Selecting a profile

Name it in the create request; omit it for the default:

```bash
curl -sS -XPOST localhost:7878/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"profile":"python-3.12","memory_mib":1024}'
# → {"id":"sbx_...","profile":"python-3.12",...}
```

An unknown profile returns `400`. The chosen profile is echoed back in the sandbox object and applies to cold `create`; forks inherit their parent snapshot's rootfs, so a snapshot taken from a `python-3.12` sandbox forks `python-3.12` children. See [api.md](api.md).

## Notes

- **Boot validation is yours to run.** The build verifies the agent and its enablement symlink are present in the image (`debugfs`), but confirming a profile *boots* means running the daemon and creating a sandbox from it on a KVM host.
- **Publishing prebuilt images** (so users download instead of build) is a distribution step tracked separately; today profiles are build-it-yourself from the pinned recipes.
