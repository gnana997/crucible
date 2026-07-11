---
title: CLI overview
description: "How the crucible client finds the daemon, how output composes in shell, and where every command lives."
---

# CLI

`crucible` is both the daemon and a thin client over its REST API. `crucible daemon` runs the server (see the [README](../README.md) and [SECURITY.md](../SECURITY.md)); every other command talks to a running daemon.

## Connecting

The client finds the daemon via `--addr` (or the `CRUCIBLE_ADDR` env var), default `127.0.0.1:7878`:

```bash
crucible --addr 127.0.0.1:7878 sandbox ls
CRUCIBLE_ADDR=10.0.0.5:7878 crucible sandbox ls
```

If the daemon has API keys configured, pass one with `--token` (or the `CRUCIBLE_TOKEN` env var). A remote daemon is served over TLS; use `--tls-skip-verify` only against a self-signed cert you trust:

```bash
CRUCIBLE_TOKEN=crucible_... crucible --addr https://vps.example:7878 sandbox ls
```

## Output

Human-readable tables by default; `-o json` on any command emits machine-readable JSON for scripts and agents:

```bash
crucible sandbox ls            # aligned table
crucible sandbox ls -o json    # JSON array
```

Commands that create a resource print its id on success, so they compose in shell:

```bash
SBX=$(crucible sandbox create --profile python-3.12)
```

## The commands

| Page | Commands |
|---|---|
| [Run and build](cli/run.md) | `run`, `build` |
| [Sandbox lifecycle](cli/lifecycle.md) | `stop`, `rm`, `shell`, `cp` |
| [Sandboxes and profiles](cli/sandboxes.md) | `sandbox create/ls/inspect/exec/rm`, `profile ls` |
| [Snapshots and fork](cli/snapshots.md) | `snapshot create/ls/inspect/rm`, `fork` |
| [Apps](cli/apps.md) | `app create/update/ls/get/rm/logs/exec/shell` |
| [Daemon, tokens, and agents](cli/daemon.md) | `daemon`, `daemon token`, `policy`, `mcp serve`, `version` |

## Exit codes

- `0`: success
- `1`: a crucible-level error (bad flags, daemon unreachable, API error)
- **the guest command's exit code** for `exec` and `run` when the command itself exits non-zero

> [!TIP]
> The exit-code passthrough makes `crucible run` and `exec` drop-in for scripts and CI: `crucible run -- pytest` fails the pipeline exactly when the tests do.
