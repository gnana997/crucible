# crucible — contributor & agent guide

crucible is an OSS Firecracker microVM sandbox runtime for AI coding agents:
fast fork/snapshot of untrusted code. A single Go binary is both the daemon and
the CLI; each guest runs a small vsock agent. Jailer isolation + snapshot/fork
with lazy `userfaultfd` memory + per-sandbox networking (netns/veth/nft/DHCP/
DNS-proxy) + clone-safety + a durable, reconciled registry. Go 1.25, Apache-2.0.

Start with `README.md`. Deeper docs live in `docs/`: [VISION](docs/VISION.md)
(why it's shaped this way), [architecture](docs/architecture.md) (how the code
fits together), [fork](docs/fork.md) (the snapshot/fork primitive), [api](docs/api.md), [wire](docs/wire.md), [cli](docs/cli.md), [mcp](docs/mcp.md),
[tui](docs/tui.md), [policy](docs/policy.md),
[profiles](docs/profiles.md), [network](docs/network.md),
[backups](docs/backups.md), [upgrades](docs/upgrades.md),
[observability](docs/observability.md), [benchmarks](docs/benchmarks.md), and
[ROADMAP](docs/ROADMAP.md) for what's next. Contribution setup is in
[CONTRIBUTING.md](CONTRIBUTING.md).

**Status:** v0.6.6 — durable apps you deploy, reach, update, pull privately, scale to zero (HTTP via the proxy **and** TCP via a wake-on-connect forwarder → self-hosted serverless postgres that snapshot-wakes in ~170 ms, no cold boot), wire together (app→app by name), scale out (N load-balanced autoscaling replicas), observe (per-app metrics + OTLP + pprof + packet capture), and give durable storage (persistent volumes for stateful sandboxes/apps — `--volume`, fsync-honest, single-writer) with point-in-time backups (`volume backup`/`restore`/`clone`, consistency-aware incl. live fsfreeze). The core runtime
is feature-complete (runtime, CLI, native rootfs profiles, `/metrics`, cgroup
quotas, install/systemd), plus OCI image boot (`crucible run <image>` / `build`),
an interactive shell + TUI, `--disk` sizing, top-level `stop`/`rm`, durable logs,
an MCP server (32 tools), daemon API-key auth with scoped/policy tokens, and a
TUI. Two durability tiers: **sandboxes** are ephemeral (a daemon restart drops
the VM), while **apps** (`crucible app`) are durable — the daemon re-creates a
healthy instance from persisted desired state. The v0.4 line built apps out:
v0.4.0 self-healing + reconcile-from-spec; v0.4.1 config (`--env`) + exec health
+ `-P` + real egress; v0.4.2 the **ingress proxy** (reach an app by name,
`web.<domain>`) + `app update` + image-`HEALTHCHECK` seeding; **v0.4.3
zero-downtime rolling `app update`** (boot → readiness gate → flip the route →
drain the old; a failed update keeps the old instance serving) + operate an app
**by name** (`app exec`/`logs`/`shell` + MCP `app_exec`/`app_logs`, resolved to
the current instance per call); **v0.4.4 private registries** (`crucible registry
login` stores a per-registry credential on the daemon that feeds every pull incl.
an app's re-pull on restart; gated by the `registry` scoped-token op; never reads
`~/.docker/config.json`; plus one-shot `run --registry-auth`); **v0.5.0
scale-to-zero** (`crucible app sleep`/`wake` + `app create --idle-timeout`
`--min-scale 0`) — an idle app snapshots to ~zero RAM and wakes **in place** on
the next request through the ingress proxy in under a second (same IP + identity,
clock stepped to now), and a slept app survives a daemon restart; **v0.5.1
app→app networking** (`--internal-networking` + `app create --can-call <other>`)
— apps reach each other by name at `<app>.internal` through the proxy VIP,
default-deny, and a scaled-to-zero callee wakes on the internal call; **v0.5.2
scale out** (`app create --min-scale N --max-scale M`) — N warm replicas
forked from a golden snapshot, P2C load-balanced by the proxy, autoscaling on
request concurrency; **v0.5.3 reliability & isolation hardening** — no orphaned
VMs across app-lifecycle edges (rolling update / delete / sleep mid-drain), the
converted-image cache is keyed by the injected agent's digest (a daemon upgrade
re-converts instead of booting a stale agent), and a published host port
coexists with the `<app>.internal` VIP on the same port (`SO_REUSEPORT`);
**v0.5.4 observability** — per-app metrics on `/metrics` (+ reference Grafana
dashboard), OTLP export of metrics + logs via `--otlp-endpoint` (an OTel
Prometheus bridge, so `/metrics` is unchanged) honoring `OTEL_*` env, daemon
`--pprof-listen`, and on-demand host-side packet capture (`sandbox`/`app
capture` → pcap, default-deny `capture` scoped op). The v0.6 line adds
persistence and serverless: **v0.6.0 persistent volumes** (`--volume NAME:/path`
on sandboxes/apps — sparse ext4 backing file, `cache_type=Writeback` so a guest
`fsync` survives a hard kill, single-writer, volume apps sleep stop/start and
redeploy destroy-then-boot; `--volume-dir` + `volume create/ls/rm` + `/volumes`
+ 3 MCP tools); **v0.6.1 wake-on-TCP** (a scale-to-zero app that publishes a port
is fronted by an app-scoped L4 forwarder that wakes it on the first TCP
connection and sleeps it on inactivity, with no proxy in the path, protocol-agnostic,
so any volume-backed database (postgres, redis, …) becomes self-hosted serverless;
`--connection-idle-timeout` reaps idle pooled connections so scale-to-zero works
for connection-pooled clients, and `--keep-connections` flips to connection-scoped
mode (reap off + TCP keepalive) for pub/sub / streaming — awake while subscribed,
asleep when nobody's connected; plus the guest init now provides `/dev/fd` for
process-substitution entrypoints). **v0.6.2** makes a volume app snapshot-sleep
and snapshot-wake like a stateless one: sleep snapshots the instance and stops the
VMM (RAM freed, single-writer guard held, backing file host-fsync'd for durability
while asleep); wake restores in place (~170 ms, same instance + IP, volume
re-attached, no cold boot / WAL recovery), and a wake after a daemon restart forks a
fresh instance from the durable snapshot re-acquiring the guard, with an automatic
stop/start cold-create fallback so a wake never fails. **v0.6.3** adds volume
backups: `volume backup`/`restore`/`clone` (a point-in-time copy restorable to a new
volume), consistency-aware (a detached/slept volume is copied directly; a live one is
FIFREEZE'd via new guest `/freeze`+`/thaw` agent ops, only the volume mount, with a
watchdog auto-thaw), reflink (O(1)) when the `--backup-dir` shares the volume
filesystem; a live backup requires a reflink FS (btrfs/XFS). **v0.6.4**
"operate with confidence": **upgrade the daemon without dropping apps** —
`app sleep --all` drains the fleet to durable snapshots, the restart re-adopts
every app as asleep, and they wake in place on demand (`docs/upgrades.md`;
`scripts/smoke_upgrade.sh` rehearses it against the previous release tag and
confirms a stateless + a volume app wake **warm** under the new binary, with an
automatic cold-create fallback if a warm cross-version wake ever fails); a
one-command **daemon backup** (`crucible admin backup` → tar.gz of the
app store, tokens, volume records, and registry creds via hot bbolt read-txns +
a manifest, gated by a default-deny `admin_backup` scoped op; restore is a
documented stopped-daemon procedure after which the reconciler re-creates every
app — volume *data* stays with `volume backup`); **disk-usage metrics**
(`snapshot_disk_bytes`/`volume_disk_bytes`/`backup_disk_bytes`, sparse-aware,
+ a Grafana disk panel + a verified latest-per-instance snapshot-retention
contract); and **IPv6 at the edge** (proxy + published ports accept v6 on a
wildcard bind and family-hop to the v4 guest; `-p '[::1]:8080:80'` pins a v6
address; guests stay v4-only). Also fixed a mid-sleep routing race (a request
racing `app sleep` could be reset; the instance is now marked non-routable
before it pauses). **v0.6.5** "capacity guards": a **sleep disk-admission floor**
(`--sleep-min-free-disk-mib`, default 1024) refuses a snapshot when free disk
under `--work-base` is low so a snapshotting fleet can't fill the disk — the app
stays running (safe degraded state), the disk complement to the existing
`--wake-min-free-mib` RAM floor, both fail-open; plus `scripts/bench_masswake.sh`,
the mass-wake load test (drain with `app sleep --all`, fire N concurrent wakes,
report the latency distribution + how gracefully the RAM floor defers to
`503`+retry) — measured 20 concurrent wakes at ~430 ms p99 with RAM barely
moving (lazy paging faults in only each guest's working set). **v0.6.6**
**off-host backups**: `volume backup export <id>` streams a backup's bytes off
the host (`GET /backups/{id}/export`, gzip by default — the sparse image's holes
compress away; `--raw` for uncompressed) and `volume backup import --source
<vol>` streams one back onto a fresh host (`POST /backups/import`), after which
`volume restore` materializes a volume — so a **remote** control plane can pull
backups over the API and ship them to an object store while the daemon stays
provider-agnostic (no cloud SDKs/creds). Both gate on a new default-deny
`volume_backup` scoped op (moves volume data; no MCP tools); backup *create*
stays snapshot-grade. See the ROADMAP for what's next (incremental backups,
TLS/ACME).

## Working style

- **When you have enough information to act, act.** Don't re-derive established
  facts, re-litigate settled decisions, or narrate options you won't pursue.
  Give a recommendation, not a survey. (This applies to user-facing messages,
  not thinking.)
- **Read symbols, not whole files.** `internal/sandbox/sandbox.go` alone is
  ~1k lines — jump to the function you need (your editor's go-to-definition,
  grep, or a code-graph tool if you have one) rather than reading files end to
  end. Keeps context lean.
- **Don't over-build.** No features, refactors, or abstractions beyond what the
  task requires. No cleanup around a bug fix, no helper for a one-shot op, no
  error handling for cases that can't happen. Validate only at system boundaries
  (HTTP input, syscalls, external processes); trust internal code.
- **Ground progress claims.** Before reporting progress, audit each claim against
  a tool result from this session. If tests fail, say so with the output; if a
  step was skipped, say that; state done-and-verified plainly, flag unverified
  explicitly. Never report fabricated status.
- **Respect boundaries.** When the ask is to describe a problem or answer a
  question, the deliverable is your assessment — report findings and stop; don't
  apply a fix until asked. Before any state-changing command (restart, delete,
  config edit), confirm the evidence supports that specific action.
- **Match the existing code.** Follow the surrounding idiom: the disciplined
  `success bool` + deferred-cleanup rollback pattern, the Firecracker
  load→vsock→rootfs→resume ordering, careful handle concurrency. New code should
  read like the code around it.

## Conventions

- **Build / test / lint:** prefer the `Makefile` targets over raw `go` when one
  exists — `make build`, `make agent`, `make test`, `make race`, `make vet`,
  `make fmt`, `make lint` (golangci-lint; config in `.golangci.yml`).
- **Git hooks:** `make hooks` installs lefthook hooks that run the CI gates
  locally — gofmt / vet / golangci-lint / build on commit, `go test -race` on
  push. CI runs the same checks; keep them green.
- **The guest agent is a separate binary** (`cmd/crucible-agent`), built static
  for `linux/amd64` (`make agent`) and baked into the rootfs. Host↔guest
  communication is over vsock (`internal/agentapi` / `internal/agentwire`);
  the shared wire shapes + frame codec live in the public `sdk/wire` package.

## Verifying

After a nontrivial change, exercise the affected flow end-to-end (drive it, don't
just typecheck) and report what you observed. `scripts/` has smoke tests
(`smoke_e2e.sh`, `smoke_fork.sh`, `smoke_clone_safety.sh`, `smoke_restart.sh`,
`smoke_mcp.sh`).
A code-review pass is warranted afterward; changes to the isolation surface —
jailer, networking, or clone-safety — additionally warrant a security review.
