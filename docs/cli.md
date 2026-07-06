# CLI

`crucible` is both the daemon and a thin client over its REST API. `crucible daemon` runs the server (see the [README](../README.md) and [SECURITY.md](../SECURITY.md)); every other command talks to a running daemon.

## Connecting

The client finds the daemon via `--addr` (or the `CRUCIBLE_ADDR` env var), default `127.0.0.1:7878`:

```bash
crucible --addr 127.0.0.1:7878 sandbox ls
CRUCIBLE_ADDR=10.0.0.5:7878 crucible sandbox ls
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

## Commands

### `crucible run [flags] -- <command>...`

One-shot: create a sandbox, run a command in it (streaming stdout/stderr), then delete it. **The command's exit code becomes crucible's exit code.**

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

### `crucible sandbox`

| Command | Description |
|---|---|
| `create [--vcpus --memory --timeout --profile --net-allow]` | create a sandbox; prints its id |
| `ls` | list live sandboxes (table: id, profile, vcpus, mem, net, age) |
| `inspect <id>` | full sandbox JSON |
| `rm <id>...` | destroy one or more sandboxes |
| `exec <id> -- <command>...` | run a command, streaming output; propagates exit code. `--cwd`, `--timeout`, `--env KEY=VALUE` |

```bash
SBX=$(crucible sandbox create --memory 1024 --profile node-22)
crucible sandbox exec $SBX --env NODE_ENV=production -- node -e 'console.log(process.version)'
crucible sandbox rm $SBX
```

Use `--` to separate the guest command from crucible's own flags.

### `crucible snapshot`

| Command | Description |
|---|---|
| `create <sandbox-id>` | snapshot a sandbox; prints the snapshot id |
| `ls` | list snapshots (table: id, source, vcpus, mem, age) |
| `inspect <id>` | full snapshot JSON |
| `rm <id>...` | delete snapshots |

### `crucible fork <snapshot-id> [--count N]`

Create `N` sandboxes (default 1) from a snapshot; prints the new sandbox ids. Each child is fully independent (its own network and, via clone-safety, its own RNG/machine identity).

```bash
SNP=$(crucible snapshot create $SBX)
crucible fork $SNP --count 5
```

### `crucible profile ls`

List the rootfs profiles the daemon was started with (`--rootfs-dir`). See [profiles.md](profiles.md).

### `crucible daemon` / `crucible version`

`daemon` runs the HTTP server (its own flags — `crucible daemon --help`). `version` prints the build version.

## Exit codes

- `0` — success
- `1` — a crucible-level error (bad flags, daemon unreachable, API error)
- **the guest command's exit code** — for `exec` and `run` when the command itself exits non-zero

This makes `crucible run`/`exec` drop-in for scripts and CI: `crucible run -- pytest` fails the pipeline exactly when the tests do.
