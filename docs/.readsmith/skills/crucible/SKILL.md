---
name: crucible
description: Crucible is a self-hosted Firecracker-microVM sandbox runtime
  (daemon + CLI/TUI/MCP clients) for running and forking untrusted or
  agent-generated code with snapshot/fork, default-deny networking, and scoped
  API tokens. Use when the user mentions crucible, Firecracker sandboxes,
  microVM isolation, sandbox snapshot/fork, crucible CLI/TUI/MCP/SDK, crucible
  daemon setup, sandbox networking allowlists, or scoped policy tokens for agent
  sandboxing.
metadata:
  readsmith-proj: crucible
  version: "1.0"
  readsmith-generated: 8b8b64c56eab77b77b0547879709d501d7d8a8a131cbde79ee6dc7b072fe18bf
---

# Crucible

## Product summary
Crucible is a self-hosted, single-host sandbox runtime that boots untrusted or agent-generated code in Firecracker microVMs under jailer, exposing one REST API (documented at https://docs.cruciblehq.dev/api) that the CLI, TUI, and MCP server all consume as thin clients. It is pre-1.0 (current v0.3.2/API 0.3.3), Apache 2.0, Go-implemented, daemon Linux-only (requires KVM); clients are cross-platform. The three load-bearing technical facts: (1) forks restore via lazy userfaultfd paging (no per-fork RAM copy) plus a copy-on-write rootfs clone, with clone-safety reseeding RNG and rotating machine-id/hostname before a fork is reachable; (2) networking is default-deny, hostname-only allowlisting enforced by host nftables + an in-daemon DNS proxy, with automatic range-filtering of RFC1918/link-local/CGNAT to block SSRF to metadata endpoints; (3) auth is off on loopback by default but required everywhere once any bearer API key exists, and keys can be bound to daemon-enforced scoped policies (operations, egress ceiling, resource caps, expiry).

## When to use
- User mentions "crucible", Firecracker microVMs, or wants isolated/sandboxed code execution for AI agents.
- Need to snapshot a warm sandbox and fork many independent copies (parallel exploration/checkpointing).
- Need to configure sandbox network egress via hostname allowlists or default-deny policy.
- Setting up `crucible daemon`, CLI (`crucible run`/`shell`/`cp`), TUI, or `crucible mcp serve`.
- Need to mint/scope API tokens/policies for agent-facing access to a crucible daemon.
- Integrating via Go/TypeScript/Python SDK or raw REST/wire protocol against a crucible daemon.
- Reproducing or reasoning about crucible's fork/snapshot performance benchmarks.

## Quick reference

| Fact | Value |
|---|---|
| Docs home | https://docs.cruciblehq.dev |
| API base URL | whatever `--listen` sets; default `http://127.0.0.1:7878` |
| Auth header | `Authorization: Bearer <key>` |
| Auth default | unauthenticated on loopback with no keys; required once any key exists |
| Non-loopback bind requirement | ≥1 API key + `--tls-cert`/`--tls-key` |
| `/healthz` | always auth-exempt, returns 200 `{"status":"ok"}` |
| Non-2xx error shape | `{"error": "message"}` |
| Client addr flag/env | `--addr` / `CRUCIBLE_ADDR`, default `127.0.0.1:7878` |
| Client token flag/env | `--token` / `CRUCIBLE_TOKEN` |
| Default sandbox | 512 MB / 1 vCPU / 60s timeout, no network |
| Frame header | 8 bytes: 1 type + 3 reserved + 4 big-endian payload length |
| Frame types | 1 stdout, 2 stderr, 3 exit, 4 stdin, 5 stdin-close |
| Max frame payload | 65536 bytes |
| Interactive exec transports | hijacked `POST .../exec?stdin=1`; WebSocket `GET .../exec` |
| Fork p50 (warm→child) | ext4 690ms vs btrfs reflink 207ms |
| Fork throughput (64-way) | ext4 3.7/s vs btrfs 45/s |
| 128-fork host RAM | ext4 4.9GB vs btrfs 1.2GB (naive copy 64GB) |
| Exec roundtrip | ~3ms (filesystem-independent) |
| Sandbox subnet | /30 from 10.20.0.0/16 pool, DNS anycast 10.20.255.254:53 |
| Concurrent sandbox cap (subnet pool) | ~16K |
| MCP transport (this release) | stdio only |
| MCP tool count | 15 |
| Prometheus metrics | `sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds` |
| GET /files default max_bytes | 10 MiB |

### Key API endpoints
| Endpoint | Purpose |
|---|---|
| `POST /sandboxes` | create sandbox (profile or image, network, publish, disk_bytes) |
| `GET/DELETE /sandboxes/{id}` | inspect / delete |
| `POST/GET /sandboxes/{id}/exec` | run command (stream) / interactive WebSocket |
| `POST/GET /sandboxes/{id}/files` | push tar / pull single file |
| `GET /sandboxes/{id}/logs` | durable logs, `since`/`source` params |
| `PUT /sandboxes/{id}/service` (Experimental), `GET /sandboxes/{id}/service` | configure supervised service / get status |
| `POST /sandboxes/{id}/service/{start,stop,restart}` | start/stop/restart supervised service |
| `GET /sandboxes/{id}/service/logs` (Experimental) | supervised service logs |
| `POST /sandboxes/{id}/snapshot` | create snapshot |
| `GET/DELETE /snapshots/{id}`, `POST /snapshots/{id}/fork?count=N` | manage snapshots / fork |
| `POST /images`, `POST /images/import`, `GET/DELETE /images/{ref}` | image pull/import/list/delete (needs `--image-dir`, else 501) |
| `GET /profiles` | list rootfs profiles |
| `GET /whoami` | effective token scope/policy |

## Decision guidance

| Choose... | When... |
|---|---|
| ext4 for `--work-base` | reflink unsupported/unavailable; accept slower, RAM-hungrier forks |
| btrfs reflink for `--work-base` | forking frequently; want O(1) clone, ~3x faster fork (207ms vs 690ms), ~4x less RAM (1.2GB vs 4.9GB) |
| XFS reflink for `--work-base` | also supports O(1) reflink clones like btrfs; no separate quantified benchmark reported |
| `pull: missing` (default) | normal repeated runs of same image |
| `pull: always` | need to pick up a moved tag |
| `pull: never` | must guarantee no network pull |
| Hijacked `?stdin=1` exec | client can hijack raw HTTP connection (Go SDK, CLI shell) |
| WebSocket exec | fetch-based clients or behind an L7 proxy |
| `crucible run <image>` | Docker-parity: long-lived entrypoint as a service |
| `crucible run -- <command>` | one-shot throwaway sandbox, exit code matters (CI/scripts) |
| `crucible stop` | halt workload, keep sandbox |
| `crucible rm` | tear down sandbox entirely |
| Scoped MCP token | agent-facing deployments — limits blast radius of theft/same-user bypass |
| Unscoped token | avoid for agents — full access even with MCP guardrails set |
| Handle methods (`snap.Fork`) | chaining sugar, no need for full response body |
| Flat methods (`Client.Fork`) | need full `SandboxResponse` incl. network details |

## Workflow

1. **Install & boot (Linux daemon host)**: `curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo bash -s -- --with-deps --enable`; verify with `crucible run nginx:alpine -p 8080:80 && curl localhost:8080`.
2. **Client-only install (macOS/Windows)**: `curl -fsSL .../install.sh | sh -s -- --client --addr https://YOUR-LINUX-HOST:7878 --token <key>`.
3. **Enable auth**: `crucible daemon token add --name <label>` (prints raw key once); require `Authorization: Bearer <key>` thereafter; rotate via `token add` + `token revoke <id>` (no restart needed).
4. **Scoped agent token**: write a policy JSON, `crucible policy validate agent-policy.json`, then `crucible daemon token add --name agent --policy agent-policy.json --ttl 24h`; inspect with `crucible --token <key> policy show`.
5. **Build & run from a Dockerfile**: `crucible build -t myapp . && crucible run myapp -p 3000:3000`.
6. **Copy in code and run it**: `SBX=$(crucible run --profile python-3.12)`; `crucible cp ./script.py $SBX:/work`; `crucible sandbox exec $SBX -- python /work/script.py`.
7. **Snapshot + fork fan-out**: `SNP=$(crucible snapshot create $SBX)`; `crucible fork $SNP --count 5` prints new sandbox ids, each with independent network/RNG/machine-id.
8. **Run MCP server for an agent**: `crucible mcp serve --default-profile python-3.12 [--allow-profiles ... --net-allow-max ... --max-sandboxes N --max-fork N --max-timeout D --tools/--deny-tools ...]`; point at a remote daemon via `--addr`/`CRUCIBLE_TOKEN`.
9. **Serve custom profiles**: build with `make agent && make profile PROFILE=<name>`, start daemon with `--rootfs-dir` pointing at `assets/profiles/`, select via `"profile"` field in create requests.
10. **Reproduce benchmarks**: `make bench`; start daemon with `CRUCIBLE_MAX_FORK=128 ... --work-base ... --chroot-base ...` on ext4 or reflink fs; run `./bin/crucible-bench --samples 50 --fanout 1,4,16,64,128 --mem-forks 128 --density 512 --json bench-results.json`.

## Common gotchas
- A daemon restart does not resurrect running sandboxes (registry/logs persist, live VMs don't) — durability is deferred to v0.4.
- ext4 has no reflink, so forking byte-copies the whole rootfs — much slower and more RAM-hungry than btrfs/XFS.
- Once any API key exists, every request needs a valid bearer token or gets 401; non-loopback bind is refused without keys + TLS.
- `network.enabled:true` with an empty/absent allowlist, or a bare `*` wildcard, is rejected with 400 — there is no full-internet option.
- Image/service/log endpoints return 501 if the daemon lacks `--image-dir`/service-capable rootfs/log store.
- Post-commit exec failures (e.g. VM dies mid-run) arrive in-band as an `exit` frame with `exit_code -1`, never as an HTTP error.
- Interactive exec (stdin=1 or WebSocket) is line-buffered with no PTY — full-screen programs/Ctrl-C job control don't work until v0.4.
- A plain GET to the exec WebSocket endpoint without an upgrade handshake returns 426.
- `usage` is `null` (not zeroed) when the process never started; `oom_killed` is a best-effort heuristic, not a precise cgroup reading.
- File push rejects tar entries that escape the destination; `GET .../files` on a directory returns 400 (single files only, capped at `max_bytes`, default 10 MiB).
- Fork is all-or-nothing: any child failing rolls back every child already started; exceeding the daemon's max fork count returns 400.
- Denied network destinations time out silently (no ICMP unreachable); denied DNS returns NXDOMAIN; IP literals never work (no DNS attestation).
- Only one networked crucible daemon can run per host (shared DNS anycast bind on 10.20.255.254:53).
- `crucible run <image>` is long-lived and NOT auto-killed — must explicitly `stop`/`rm`.
- An unscoped API key still grants full access even if MCP `--tools`/`--max-*` guardrails are set; policies are immutable (no `token edit`, only add+revoke).
- The TypeScript SDK is not on npm yet; Python SDK has generated models but no client layer yet — check before assuming availability.
- Nested virtualization on Apple Silicon is unreliable — don't run the daemon locally on a Mac; run it on a Linux host instead.

## Verification checklist
- [ ] Daemon reachable: `GET /healthz` (or `crucible daemon token list`/`whoami`) returns success before other calls.
- [ ] Correct `--addr`/`CRUCIBLE_ADDR` and `--token`/`CRUCIBLE_TOKEN` used for the target daemon (loopback vs remote).
- [ ] If binding non-loopback, confirmed at least one API key exists and `--tls-cert`/`--tls-key` are set.
- [ ] Network config explicitly sets `network.enabled` and a non-empty, non-wildcard allowlist if egress is needed; otherwise sandbox correctly has no network.
- [ ] For fork workflows, snapshot id was captured before calling `fork`, and `count` respects the daemon's max fork cap.
- [ ] Exec results checked for `timed_out`/`oom_killed`/`signal`/non-null `usage`, not just `exit_code`.
- [ ] File push (`putFiles`) checked for path-escape rejection; file pulls confirmed within `max_bytes` cap.
- [ ] Chosen filesystem (ext4 vs btrfs/XFS reflink) matches performance expectations for fork-heavy workloads.
- [ ] For agent-facing deployments, a scoped (not unscoped) token was minted and validated via `crucible policy validate`.
- [ ] Ephemeral-sandbox assumption respected: no reliance on sandboxes surviving a daemon restart pre-v0.4.

## Resources
- Product overview: https://docs.cruciblehq.dev
- REST API guide: https://docs.cruciblehq.dev/api
- API reference index: https://docs.cruciblehq.dev/api-reference
- Architecture: https://docs.cruciblehq.dev/architecture
- Benchmarks: https://docs.cruciblehq.dev/benchmarks
- CLI: https://docs.cruciblehq.dev/cli
- MCP server: https://docs.cruciblehq.dev/mcp
- Networking: https://docs.cruciblehq.dev/network
- Policy/scoped tokens: https://docs.cruciblehq.dev/policy
- Profiles: https://docs.cruciblehq.dev/profiles
- Roadmap: https://docs.cruciblehq.dev/roadmap
- TUI: https://docs.cruciblehq.dev/tui
- Vision: https://docs.cruciblehq.dev/vision
- Wire protocol: https://docs.cruciblehq.dev/wire
- SDKs: https://docs.cruciblehq.dev/sdks/overview, https://docs.cruciblehq.dev/sdks/go, https://docs.cruciblehq.dev/sdks/python, https://docs.cruciblehq.dev/sdks/typescript
- Key API reference pages: https://docs.cruciblehq.dev/api-reference/health, https://docs.cruciblehq.dev/api-reference/createsandbox, https://docs.cruciblehq.dev/api-reference/execsandbox, https://docs.cruciblehq.dev/api-reference/execsandboxws, https://docs.cruciblehq.dev/api-reference/createsnapshot, https://docs.cruciblehq.dev/api-reference/forksnapshot, https://docs.cruciblehq.dev/api-reference/whoami
