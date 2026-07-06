# crucible — contributor & agent guide

crucible is an OSS Firecracker microVM sandbox runtime for AI coding agents:
fast fork/snapshot of untrusted code. A single Go binary is both the daemon and
the CLI; each guest runs a small vsock agent. Jailer isolation + snapshot/fork
with lazy `userfaultfd` memory + per-sandbox networking (netns/veth/nft/DHCP/
DNS-proxy) + clone-safety + a durable, reconciled registry. Go 1.25, Apache-2.0.

Start with `README.md`. Deeper docs live in `docs/`: [VISION](docs/VISION.md)
(why it's shaped this way), [architecture](docs/architecture.md) (how the code
fits together), [api](docs/api.md), [cli](docs/cli.md), [mcp](docs/mcp.md),
[profiles](docs/profiles.md), [network](docs/network.md), and
[ROADMAP](docs/ROADMAP.md) for what's next. Contribution setup is in
[CONTRIBUTING.md](CONTRIBUTING.md).

**Status:** the v0.1 core runtime is feature-complete — runtime, CLI, native
rootfs profiles, a Prometheus `/metrics` endpoint, host cgroup quotas, and an
install script + systemd unit. See the ROADMAP for what's planned next.

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
  communication is over vsock (`internal/agentapi` / `internal/agentwire`).

## Verifying

After a nontrivial change, exercise the affected flow end-to-end (drive it, don't
just typecheck) and report what you observed. `scripts/` has smoke tests
(`smoke_e2e.sh`, `smoke_fork.sh`, `smoke_clone_safety.sh`, `smoke_restart.sh`,
`smoke_mcp.sh`).
A code-review pass is warranted afterward; changes to the isolation surface —
jailer, networking, or clone-safety — additionally warrant a security review.
