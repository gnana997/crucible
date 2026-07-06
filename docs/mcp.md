# MCP

`crucible mcp serve` exposes crucible to any MCP-compatible agent (Claude Code, Cursor, …) as **native tools** — create a sandbox, run code, snapshot, fork — with no shell wrapping and no SDK.

It is a thin client: every tool call becomes one typed call against the daemon's REST API, so an MCP tool and the equivalent CLI command hit the identical code path and can't drift. The server owns no sandbox state — everything lives in the daemon.

```
  agent (Claude Code / Cursor)
    │  MCP / JSON-RPC over stdio
    ▼
  crucible mcp serve        ← thin wrapper over the daemon client
    │  HTTP  (--addr, --token)
    ▼
  crucible daemon
    ▼
  Firecracker microVMs
```

## Transport

**stdio only** in this release. The agent spawns `crucible mcp serve` as a subprocess and speaks JSON-RPC over its stdin/stdout. No network surface is required.

Transport and daemon location are independent: because the server is a client, "stdio" is **not** "local only". Point `--addr` at a remote daemon and the same local subprocess bridges to it — that covers both "agent + daemon on one host" and "local agent → hosted crucible".

A directly network-reachable MCP endpoint (Streamable HTTP + MCP OAuth) is a later addition.

## Agent configuration

Point the agent's MCP config at the command. A Claude Code / Cursor `mcpServers` entry:

```json
{
  "crucible": {
    "command": "crucible",
    "args": ["mcp", "serve", "--default-profile", "python-3.12"]
  }
}
```

For a remote daemon, add the address and key (served over TLS — see [Authentication](#authentication)):

```json
{
  "crucible": {
    "command": "crucible",
    "args": ["mcp", "serve", "--addr", "https://crucible.example:7878"],
    "env": { "CRUCIBLE_TOKEN": "crucible_…" }
  }
}
```

## Tools

Each tool is a thin wrapper over one daemon call. Names are snake_case per MCP convention.

| Tool | What it does | Key inputs |
|---|---|---|
| `run` | The 80% case: create a sandbox, run one command, return its output, **always delete** it. | `command` (argv), `profile?`, `env?`, `timeout_s?`, `net_allow?[]` |
| `create_sandbox` | Create a persistent sandbox. | `profile?`, `vcpus?`, `memory_mib?`, `timeout_s?`, `net_allow?[]` |
| `exec` | Run a command in an existing sandbox; capture and return. | `sandbox_id`, `command`, `cwd?`, `env?`, `timeout_s?` |
| `snapshot` | Snapshot a sandbox's warm state. | `sandbox_id` |
| `fork` | Create N independent, clone-safe sandboxes from a snapshot. | `snapshot_id`, `count?` |
| `list_sandboxes` | List live sandboxes. | — |
| `inspect_sandbox` | Full detail for one sandbox. | `sandbox_id` |
| `delete_sandbox` | Destroy a sandbox. | `sandbox_id` |
| `list_snapshots` | List snapshots. | — |
| `delete_snapshot` | Delete a snapshot. | `snapshot_id` |
| `list_profiles` | List available rootfs profiles. | — |

- **`run` is the star** — most agents want "run this code, give me the output" with no lifecycle to manage. `env` entries are `KEY=VALUE` strings; `net_allow` is a list of hostnames the sandbox may reach.
- **The primitives (`create`/`exec`/`snapshot`/`fork`)** are what make crucible special: an agent can set up once, branch N ways, and keep the best.
- **`exec` is capture-and-return** — the full result (`exit_code`, `stdout`, `stderr`, `timed_out`, `oom_killed`, `duration_ms`). Live streaming and interactive REPLs are not in this release.
- **Errors** surface as MCP tool errors carrying the daemon's message (e.g. unknown profile, sandbox not found), so the agent gets something actionable.

## Security model

MCP changes the threat model. The Firecracker VM already protects the **host**; what's new is that MCP hands an **LLM agent** the ability to spawn VMs and run code. If the agent is **prompt-injected** (a poisoned page, a malicious file it reads), the attacker inherits the agent's crucible capability. The guardrails exist to **bound what the agent can do**, on the assumption the agent may be turned against you.

**Core principle:** the *operator* sets policy with the flags below at launch; the *agent* operates strictly within it and can never expand its own privileges — an LLM can't rewrite the server's flags.

### Network — the highest-risk axis

Egress is where an injected agent does real damage (exfiltrate secrets, phone home, SSRF). So:

- **Default: no network.** A sandbox with no `net_allow` gets nothing.
- **The agent controls `net_allow`** on `run` / `create_sandbox` — blocking it entirely would cripple legitimate use (`pip install`, `npm ci`, fetching docs).
- **crucible's protections don't care who set the allowlist:** it stays default-deny, and resolved IPs in link-local / RFC1918 / CGNAT are still range-filtered out — so even an agent-chosen allowlist **cannot reach cloud-metadata (169.254.169.254) or internal services**. Agent-controlled egress opens only *public* hosts the agent names.
- **`--net-allow-max <host>…`** (optional) caps it: the agent's `net_allow` must be a subset of this list. For deployments that can't accept agent-chosen public egress.

### Operator flags

| Flag | Default | Purpose |
|---|---|---|
| `--default-profile <name>` | — | profile used when a tool omits one (needed for `run` without an explicit profile) |
| `--allow-profiles <list>` | all | restrict which rootfs the tools may launch |
| `--net-allow-max <list>` | unset | ceiling on agent-chosen egress (subset check) |
| `--max-sandboxes <n>` | `8` | max concurrent live sandboxes (best-effort) |
| `--max-fork <n>` | `8` | cap the `fork` tool's count |
| `--max-timeout <dur>` | `300s` | clamp every `run` / `exec` command timeout |
| `--tools <list>` | all | expose only these tools |
| `--deny-tools <list>` | none | hide these tools (e.g. drop `fork` / `delete_*`) |

A tool removed by `--tools` / `--deny-tools` is never registered, so it never appears in the agent's `tools/list`.

## Authentication

The MCP server reaches the daemon as a normal client, so it uses the daemon's [bearer-key auth](api.md#authentication):

- **Local daemon over loopback:** no credential needed (loopback trust).
- **Remote daemon:** the daemon must require a key and serve TLS; pass the key with `--token` or `CRUCIBLE_TOKEN`. A bearer token over plaintext HTTP is a leaked token, which is why binding a non-loopback address without TLS is refused daemon-side.

## Limitations

**The local same-user bypass (honest, and narrow).** Locally, the daemon key sits in a file your OS user can read. A same-user agent that *also* has a shell tool could read that key and hit the loopback daemon directly, past this server's guardrails. Bearer keys make the *remote* story solid but don't bound this. Treat a local key like any credential: on a shared box, protect it.

Why not mTLS / workload identity? Those answer *"is this client who it claims to be?"* — but the MCP server and the agent's other tools share the *same* OS user, so they have the same identity. You can't hide a secret from a same-user process.

The real fix (future) is **scoped tokens**: a token bound to a policy the *daemon* enforces (no network, N sandboxes, these tools). Then a stolen token buys only the capability it already had — the bypass gains nothing, and these MCP guardrails become convenience rather than the security boundary. Until then, the bound above is the honest ceiling.

See [SECURITY.md](../SECURITY.md) for the isolation model and how to report a vulnerability.
