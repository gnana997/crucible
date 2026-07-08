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
CRUCIBLE_TOKEN=crucible_… crucible --addr https://vps.example:7878 sandbox ls
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

### `crucible run`

Two shapes, chosen by the `--` separator:

**Image mode — `crucible run <image> [flags]`** (the docker-parity headline). Boots an OCI image as a sandbox: its entrypoint runs as the service. Prints the sandbox id (stdout). **Long-lived by default — it is *not* auto-killed;** stop it with `crucible stop <id>` or remove it with `crucible rm <id>`. The image is acquired the same way as `sandbox create --image` (a locally-built Docker tag is imported client-side; otherwise the daemon resolves it from its store or a registry under `--pull`).

| Flag | Meaning |
|---|---|
| `-p, --publish` (repeatable) | publish a port `[HOST_IP:]HOST:GUEST[/tcp]` |
| `--net-allow` (repeatable) | allowlisted hostname; enables egress |
| `--pull` | `missing` (default) / `always` / `never` |
| `--rm` | tail logs in the foreground; remove the sandbox on detach (Ctrl-C) |
| `--vcpus`, `--memory`, `--timeout` | sizing / deadline (`--timeout 0` = long-lived) |

```bash
crucible run nginx:alpine -p 8080:80          # boot, publish, leave running
crucible build -t myapp . && crucible run myapp -p 3000:3000
crucible run alpine:latest --rm               # foreground; removed on Ctrl-C
```

**Command mode — `crucible run [flags] -- <command>...`**. One-shot: create a throwaway sandbox (a `--profile`, or the daemon default), run one command (streaming stdout/stderr), then delete it. **The command's exit code becomes crucible's exit code.**

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

### `crucible build [-t <tag>] [-f <Dockerfile>] <context>`

Build a Dockerfile locally (`docker build`) and load the result into crucible's image store in one verb; prints the converted image digest for `crucible run` / `sandbox create --image`. Docker is a **client-side** convenience — the daemon never needs it.

```bash
crucible build -t myapp .                 # prints sha256:… (in the store)
crucible run "$(crucible build .)" -p 8080:80
```

### `crucible stop <id>...` and `crucible rm <id>...`

`stop` gracefully stops a sandbox's entrypoint (image StopSignal → grace → SIGKILL) while **keeping** the sandbox — the ops "pull the plug on the workload" action. `rm` (alias `delete`) **removes** the sandbox (hard kill), the same as `sandbox rm`. Both are top-level for docker-parity muscle memory.

```bash
crucible stop sbx_abc      # halt the workload, keep the box
crucible rm sbx_abc        # tear the box down
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

**API keys.** `crucible daemon token` manages the daemon's bearer keys (stored hashed in `--token-file`, default `/var/lib/crucible/tokens.json`):

```bash
crucible daemon token add --name laptop                       # unscoped (full access), prints the key once
crucible daemon token add --name agent --policy p.json --ttl 24h  # scoped + expiring
crucible daemon token list                  # id, name, scope, expiry — never the key
crucible daemon token revoke <id>           # rotate = add a new key, then revoke the old
```

With no keys, a loopback daemon serves unauthenticated. Once any key exists, auth is required. Binding a non-loopback `--listen` is refused unless keys and `--tls-cert`/`--tls-key` are both set. `--policy` binds a key to a [scoped policy](policy.md) the daemon enforces; `--ttl` sets an expiry. See [SECURITY.md](../SECURITY.md) and [api.md](api.md#authentication).

### `crucible policy`

Author and inspect [scoped-token policies](policy.md):

```bash
crucible policy validate p.json    # static check — the same validation token add runs (fail-closed)
crucible policy show               # what the current --token may actually do (asks the daemon /whoami)
```

`policy validate` reads a file or `-` (stdin). `policy show -o json` emits the effective policy for scripting.

### `crucible mcp serve`

Runs a stdio MCP server so any MCP agent (Claude Code, Cursor, …) can drive crucible as native tools. It bridges to the daemon at `--addr` (with `--token`), so it works against a local or a remote daemon. Operator guardrails (`--default-profile`, `--allow-profiles`, `--net-allow-max`, `--max-sandboxes`, `--max-fork`, `--max-timeout`, `--tools`/`--deny-tools`) bound what the agent can do. Full reference and the agent config example are in [docs/mcp.md](mcp.md).

## Exit codes

- `0` — success
- `1` — a crucible-level error (bad flags, daemon unreachable, API error)
- **the guest command's exit code** — for `exec` and `run` when the command itself exits non-zero

This makes `crucible run`/`exec` drop-in for scripts and CI: `crucible run -- pytest` fails the pipeline exactly when the tests do.
