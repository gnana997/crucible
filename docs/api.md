---
title: HTTP API
description: "Base URL, authentication, error conventions, and a complete example session. The endpoint-by-endpoint contract lives in the generated API reference."
---

# HTTP API

crucible is driven entirely over HTTP. The endpoint-by-endpoint contract is in the [API reference](/api-reference), generated from the same OpenAPI document the daemon is tested against; this page covers the things that apply to every call.

- **Base URL:** whatever you pass to `--listen` (default `http://127.0.0.1:7878`).
- **Content type:** requests and responses are JSON, except the [exec stream](api/exec.md). Request bodies are size-capped; oversized bodies are rejected.
- **Errors:** any non-2xx JSON response has the shape `{"error": "message"}`.
- **IDs are validated on every path**; a malformed sandbox or snapshot ID returns `400` before any work happens.
- **Optional subsystems answer `501`** when the daemon runs without them (`--image-dir` for images, a service-capable rootfs, `--log-dir` for logs, `--app-db` for apps). A known path with an unsupported method returns `405`.

## Authentication

The daemon supports bearer-token API keys, on the same model as the Docker or Kubernetes CLIs.

- **Loopback default: no auth.** With no keys configured, the daemon (bound to `127.0.0.1`) serves every request unauthenticated: the single-operator local case.
- **Enabling auth.** Create a key with `crucible daemon token add --name <label>`. The raw key is printed once (`crucible_...`); only its SHA-256 hash is stored. As soon as one key exists, every request must carry `Authorization: Bearer <key>` or it is rejected `401` with a `WWW-Authenticate: Bearer` header. Manage keys with `crucible daemon token list` / `revoke <id>`; changes take effect without a daemon restart.
- **`/healthz` is always exempt**, so liveness probes work without a key.
- **Binding non-loopback requires auth plus TLS.** The daemon refuses a non-loopback `--listen` unless at least one key exists and `--tls-cert`/`--tls-key` are set. Clients send the key over TLS with `--token` / `CRUCIBLE_TOKEN`.

> [!TIP]
> A key can be bound to a [scoped policy](policy.md) with `daemon token add --policy`, narrowing what it may do; `--ttl` adds an expiry.

## Metrics

`GET /metrics` serves Prometheus metrics in the standard text format, label-free today:

| Metric | Type | Meaning |
|---|---|---|
| `sandboxes_created_total` | counter | Sandboxes created (cold boots + forks) |
| `sandboxes_active` | gauge | Sandboxes currently live |
| `fork_duration_seconds` | histogram | End-to-end time to bring up one fork |
| `snapshot_restore_duration_seconds` | histogram | Time to restore a VM from a snapshot |

Like the rest of the API, `/metrics` is subject to auth when keys are configured and is loopback-bound by default: scrape it from a colocated agent, not across a trust boundary.

## Example session

```bash
# create
SBX=$(curl -s -XPOST localhost:7878/sandboxes -d '{"memory_mib":1024}' | jq -r .id)

# run setup
curl -s -XPOST localhost:7878/sandboxes/$SBX/exec \
  -d '{"cmd":["sh","-lc","git clone https://github.com/x/y /app && cd /app && pip install -r requirements.txt"]}' --output -

# snapshot after setup
SNP=$(curl -s -XPOST localhost:7878/sandboxes/$SBX/snapshot | jq -r .id)

# fork 4 children from the warm snapshot
curl -s -XPOST "localhost:7878/snapshots/$SNP/fork?count=4" | jq '.sandboxes[].id'
```

The exec response is a binary frame stream, specified language-neutrally in [the wire protocol](wire.md) with conformance fixtures under `sdks/fixtures`. The [SDKs](sdks/overview.md) wrap all of this.
