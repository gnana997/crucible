---
title: Daemon, tokens, and agents
description: "Run the server, manage bearer keys with daemon token, author scoped policies, and expose crucible to MCP agents."
---

# Daemon, tokens, and agents

## `crucible daemon` and `crucible version`

`daemon` runs the HTTP server (its own flags: `crucible daemon --help`). `version` prints the build version.

## API keys: `crucible daemon token`

The daemon's bearer keys are stored hashed in `--token-file` (default `/var/lib/crucible/tokens.json`):

```bash
crucible daemon token add --name laptop                            # unscoped (full access), prints the key once
crucible daemon token add --name agent --policy p.json --ttl 24h   # scoped + expiring
crucible daemon token list                  # id, name, scope, expiry; never the key
crucible daemon token revoke <id>           # rotate = add a new key, then revoke the old
```

> [!IMPORTANT]
> With no keys, a loopback daemon serves unauthenticated. Once any key exists, auth is required everywhere. Binding a non-loopback `--listen` is refused unless keys and `--tls-cert`/`--tls-key` are both set.

`--policy` binds a key to a [scoped policy](../policy.md) the daemon enforces; `--ttl` sets an expiry. See [SECURITY.md](../../SECURITY.md) and the [HTTP API](../api.md#authentication).

## `crucible policy`

Author and inspect [scoped-token policies](../policy.md):

```bash
crucible policy validate p.json    # static check; the same validation token add runs (fail-closed)
crucible policy show               # what the current --token may actually do (asks the daemon /whoami)
```

`policy validate` reads a file or `-` (stdin). `policy show -o json` emits the effective policy for scripting.

## `crucible mcp serve`

Runs a stdio MCP server so any MCP agent (Claude Code, Cursor, and friends) can drive crucible as native tools. It bridges to the daemon at `--addr` (with `--token`), so it works against a local or a remote daemon.

Operator guardrails bound what the agent can do: `--default-profile`, `--allow-profiles`, `--net-allow-max`, `--max-sandboxes`, `--max-fork`, `--max-timeout`, and `--tools`/`--deny-tools`. The full reference and the agent config example are in [MCP](../mcp.md).
