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
[benchmarks](docs/benchmarks.md), and
[ROADMAP](docs/ROADMAP.md) for what's next. Contribution setup is in
[CONTRIBUTING.md](CONTRIBUTING.md).

**Status:** v0.6.0 — durable apps you deploy, reach, update, pull privately, scale to zero, wire together (app→app by name), scale out (N load-balanced autoscaling replicas), observe (per-app metrics + OTLP + pprof + packet capture), and give durable storage (persistent volumes for stateful sandboxes/apps — `--volume`, fsync-honest, single-writer). The core runtime
is feature-complete (runtime, CLI, native rootfs profiles, `/metrics`, cgroup
quotas, install/systemd), plus OCI image boot (`crucible run <image>` / `build`),
an interactive shell + TUI, `--disk` sizing, top-level `stop`/`rm`, durable logs,
an MCP server (30 tools), daemon API-key auth with scoped/policy tokens, and a
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
capture` → pcap, default-deny `capture` scoped op). See the ROADMAP for what's
next.

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
