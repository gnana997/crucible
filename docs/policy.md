---
title: Scoped tokens and policies
description: "Bind an API key to a policy the daemon enforces on every request: a small, bounded, revocable capability for agents and CI."
---

# Scoped tokens & policies

A crucible API key can be **scoped** to a policy the **daemon** enforces on every
request. A scoped, expiring key is a small, bounded, revocable capability — so
handing one to an agent (or exposing a remote daemon) is safe: a leaked or
stolen scoped token is *worthless beyond its policy*.

This is what makes the MCP guardrails a real boundary rather than a convenience:
enforcement lives in the authoritative daemon, so an agent that bypasses its MCP
server and calls the daemon directly gets exactly the bounded capability it
already had.

- **Unscoped key** (or loopback with no keys): full access — the default.
- **Scoped key**: the daemon rejects anything outside its policy.

Existing keys are unscoped, so nothing changes until you mint a scoped one.

## The workflow

```bash
# 1. author a policy (JSON) and check it — same validation the daemon uses
crucible policy validate agent-policy.json

# 2. mint a scoped, expiring key (fails closed if the policy is invalid)
crucible daemon token add --name agent --policy agent-policy.json --ttl 24h

# 3. from the client side, see what a token may actually do
crucible --token crucible_… policy show
```

`policy validate` runs the *exact* function that gates `token add`, so a file
that validates here can't be rejected at mint time. Both check statically
(schema, types, patterns); when `policy validate` can reach a daemon it will
later also verify live facts like profile existence.

## Policy schema

A small JSON object. **Every field is optional; absent means "no restriction on
that axis"**, so `{}` is fully permissive.

| Field | Type | Meaning |
|---|---|---|
| `operations` | `[]string` | allow-list of verbs: `create`, `exec`, `snapshot`, `fork`, `delete`, `read`, `registry`, `capture`, `admin_backup`. Absent/empty = all allowed. |
| `net_allow_max` | `[]string` | hostname egress ceiling (tri-state, below). |
| `net_full_egress` | `bool` | grants the range-based egress modes (`full_egress`, `allowlist_cidr`). Default `false`: without it, a request asking for either is rejected — so a `net_allow_max` hostname ceiling can't be bypassed by switching to full-egress. |
| `allow_profiles` | `[]string` | which rootfs profiles may launch. Absent = any. |
| `max_sandboxes` | `int` | concurrent live sandboxes (0 = unlimited). |
| `max_fork` | `int` | cap on a single `fork` count (0 = unlimited). |
| `max_timeout_s` | `int` | clamp on every run/exec command timeout (0 = no clamp). |
| `max_vcpus` | `int` | cap on a create's vCPU count (0 = unlimited). |
| `max_memory_mib` | `int` | cap on a create's memory (0 = unlimited). |

**`net_allow_max` is tri-state** — the one field with meaningful emptiness:

| Value | Meaning |
|---|---|
| absent | no restriction — the agent may allowlist any public host (the daemon's range-filter still blocks internal/metadata addresses) |
| `[]` (present, empty) | **no network at all** — every request's `net_allow` must be empty |
| `["pypi.org","*.npmjs.org"]` | a request's `net_allow` must be a **subset** (normalized exact match) |

Host patterns use the same syntax as network allowlists (`docs/network.md`):
exact hostnames or a single leading-label wildcard (`*.npmjs.org`); bare `*` is
rejected.

## Operations ↔ endpoints ↔ tools

The daemon speaks endpoints; MCP speaks tools. Both map to the same operations,
so the MCP server can mirror a policy by advertising only the tools it permits.

| Operation | Endpoints | MCP tools that need it |
|---|---|---|
| `create` | `POST /sandboxes` | `run`, `create_sandbox` |
| `exec` | `POST /sandboxes/{id}/exec` | `run`, `exec` |
| `snapshot` | `POST /sandboxes/{id}/snapshot` | `snapshot` |
| `fork` | `POST /snapshots/{id}/fork` | `fork` |
| `delete` | `DELETE /sandboxes|snapshots/{id}` | `run`, `delete_sandbox`, `delete_snapshot` |
| `read` | all `GET` | `list_sandboxes`, `inspect_sandbox`, `list_snapshots`, `list_profiles` |
| `registry` | `POST`/`DELETE /registry/credentials` | *(none — operator action, no MCP tool)* |
| `capture` | `GET /sandboxes/{id}/capture` | `capture` — **default-deny; grant explicitly.** Packet capture exposes traffic payloads, so it is never implied by `read`. |
| `admin_backup` | `GET /admin/backup` | *(none — operator action, no MCP tool)* — **default-deny; grant explicitly.** The daemon backup streams token state and usable registry secrets. |

`run` creates → execs → deletes in one call, so it needs `create`+`exec`+`delete`
— a token lacking any of those won't be offered `run`.

## Worked examples

**A sandboxed code runner** — run untrusted code, fetch from PyPI, nothing else:

```json
{
  "operations": ["create", "exec", "delete", "read"],
  "allow_profiles": ["python-3.12"],
  "net_allow_max": ["pypi.org", "*.pythonhosted.org"],
  "max_sandboxes": 4,
  "max_timeout_s": 120,
  "max_vcpus": 2,
  "max_memory_mib": 1024
}
```

**Air-gapped** — compute only, never any network:

```json
{ "operations": ["create", "exec", "delete"], "net_allow_max": [] }
```

**Read-only observer** — inspect, never mutate:

```json
{ "operations": ["read"] }
```

## Enforcement

- **The daemon is authoritative.** It checks the presenting token's policy on
  every request: the operation verb (403 if not allowed), and — on create/exec/
  fork — the resource ceilings. Every violation on one request is reported at
  once. An expired token is rejected with `401`.
- **`GET /whoami`** returns the effective policy (`crucible policy show`), so a
  client can discover exactly what it may do. It carries no operation gate — even
  a token with an empty `operations` list can introspect itself.
- **The MCP server mirrors the policy.** At startup it calls `/whoami` and
  advertises only the tools the policy permits, so an agent never sees a tool it
  can't use. This is UX only — the daemon still enforces, so a hidden-but-called
  tool is still rejected.
- **`mcp serve` flags narrow, never widen.** The operator's `--max-*` /
  `--net-allow-max` on `mcp serve` layer *on top of* the token policy: a request
  must satisfy both. They can only tighten what a scoped token already allows.

## Immutability & rotation

A token's policy is **immutable** — there is no `token edit`. To change a
policy, rotate: `token add` a new scoped key, then `token revoke` the old one.

## Limitations

- **The local same-user bypass is closed for scoped tokens.** A same-user agent
  that steals its MCP server's key and calls the loopback daemon directly gets
  only the token's bounded capability — the bypass gains nothing. (An *unscoped*
  key still grants full access; scope the keys you expose.)
- **`max_sandboxes` is counted per token** — each scoped token has its own
  budget, so two tokens can't consume each other's. (Best-effort: the live count
  races a concurrent create, which is fine for a single agent.)
- **Short-lived session tokens** (an MCP server exchanging its long-lived key for
  a scoped, short-TTL session token at startup) are deferred; `--ttl` on the key
  itself is the expiry mechanism today.

See [SECURITY.md](../SECURITY.md) for the isolation model and [docs/mcp.md](mcp.md)
for how the MCP server consumes all this.
