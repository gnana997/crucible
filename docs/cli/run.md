---
title: Run and build
description: "crucible run in its two shapes (boot an image, or run one command and vanish) and crucible build, the local Dockerfile-to-store shortcut."
---

# Run and build

`crucible run` has two shapes, chosen by the `--` separator: boot an image as a long-lived sandbox, or run one command in a throwaway sandbox.

## Image mode

`crucible run <image> [flags]` is the docker-parity headline. It boots an OCI image as a sandbox: the image's entrypoint runs as the service, and the sandbox id prints to stdout. The image is acquired the same way as `sandbox create --image`: a locally-built Docker tag is imported client-side; otherwise the daemon resolves it from its store or a registry under `--pull`.

> [!IMPORTANT]
> Image-mode sandboxes are long-lived by default. Nothing auto-kills them: stop one with `crucible stop <id>` or remove it with `crucible rm <id>`.

| Flag | Meaning |
|---|---|
| `-p, --publish` (repeatable) | publish a port `[HOST_IP:]HOST:GUEST[/tcp]` |
| `-P, --publish-all` | publish every port the image `EXPOSE`s (guest N to host N) |
| `--net-allow` (repeatable) | allowlisted hostname; enables egress |
| `--net-allow-cidr` (repeatable) | allow direct egress to a public IPv4 CIDR (e.g. `203.0.113.0/24`) |
| `--net-full-egress` | reach any public host (metadata/link-local/RFC1918 still blocked) |
| `--pull` | `missing` (default) / `always` / `never` |
| `--disk` | grow the writable rootfs to this size, e.g. `2G` / `512M` (default: template headroom) |
| `--rm` | tail logs in the foreground; remove the sandbox on detach (Ctrl-C) |
| `--vcpus`, `--memory`, `--timeout` | sizing / deadline (`--timeout 0` = long-lived) |

```bash
crucible run nginx:alpine -p 8080:80          # boot, publish, leave running
crucible build -t myapp . && crucible run myapp -p 3000:3000
crucible run alpine:latest --rm               # foreground; removed on Ctrl-C
```

## Command mode

`crucible run [flags] -- <command>...` is one-shot: create a throwaway sandbox (a `--profile`, or the daemon default), run one command streaming stdout and stderr, then delete it. The command's exit code becomes crucible's exit code.

| Flag | Meaning |
|---|---|
| `--profile` | rootfs profile (e.g. `python-3.12`) |
| `--vcpus`, `--memory`, `--timeout` | sizing / deadline |
| `--net-allow` (repeatable) | allowlisted hostname; enables networking |
| `--keep` | keep the sandbox instead of deleting it |

```bash
crucible run --profile python-3.12 -- python -c 'print(2**10)'
crucible run --net-allow pypi.org --net-allow '*.pythonhosted.org' -- pip download requests
```

## `crucible build`

`crucible build [-t <tag>] [-f <Dockerfile>] <context>` builds a Dockerfile locally (`docker build`) and loads the result into crucible's image store in one verb; it prints the converted image digest for `crucible run` or `sandbox create --image`.

> [!NOTE]
> Docker is a client-side convenience here. The daemon never needs it.

```bash
crucible build -t myapp .                 # prints sha256:... (in the store)
crucible run "$(crucible build .)" -p 8080:80
```
