# crucible — agent guide

crucible is an OSS Firecracker microVM sandbox runtime for AI coding agents
(fast fork/snapshot of untrusted code). Single Go binary, HTTP API, jailer +
snapshot/fork + per-sandbox networking (netns/veth/nft/DHCP/DNS-proxy) +
vsock agent. Go 1.25, Apache-2.0. See `docs/VISION.md` and `docs/ROADMAP.md`.

**Current focus:** closing the five gaps in `docs/GAPS.md` that stand between the
runtime and real single-node fork density → eventual multi-host orchestration.

## Working style

- **When you have enough information to act, act.** Don't re-derive established
  facts, re-litigate settled decisions, or narrate options you won't pursue.
  Give a recommendation, not a survey. (This applies to user-facing messages,
  not thinking.)
- **Explore with the codebase-memory (cbm) tools, not by reading whole files.**
  Use `search_graph` to locate a symbol, `get_code_snippet` to read just that
  function, `trace_path` for call chains, `detect_changes` to review the blast
  radius of your edits, `query_graph` for impact analysis. Read symbols, not
  files — `internal/sandbox/sandbox.go` alone is ~940 lines. This keeps context
  lean and is the intended review path.
- **Don't over-build.** No features, refactors, or abstractions beyond what the
  task requires. No cleanup around a bug fix, no helper for a one-shot op, no
  error handling for cases that can't happen. Validate only at system boundaries
  (HTTP input, syscalls, external processes); trust internal code.
- **Ground progress claims.** Before reporting progress, audit each claim against
  a tool result from this session. If tests fail, say so with the output; if a
  step was skipped, say that; state done-and-verified plainly, flag unverified
  explicitly. Never report fabricated status.
- **Respect boundaries.** When I'm describing a problem or asking a question, the
  deliverable is your assessment — report findings and stop; don't apply a fix
  until I ask. Before any state-changing command (restart, delete, config edit),
  confirm the evidence supports that specific action.
- **Match the existing code.** Follow the surrounding idiom: the disciplined
  `success bool` + deferred-cleanup rollback pattern, the Firecracker
  load→vsock→rootfs→resume ordering, careful handle concurrency. New code should
  read like the code around it.

## Conventions

- Build/lint/test: see `Makefile` and `.golangci.yml`. Run `make` targets rather
  than raw `go` when one exists.
- Reference material for the hard gaps lives in `../study-material/`
  (`userfaultfd-internals.md` + `uffd_lab.c` for gap 1; `network-internals.md`
  for the network stack). Read these before touching those subsystems.
- Append durable gotchas you discover to `docs/lessons.md` — one lesson per
  entry, with a one-line summary and *why it mattered*. Update an existing entry
  rather than duplicating; delete notes that turn out wrong.

## Verifying

After a nontrivial change, exercise the affected flow end-to-end (drive it, don't
just typecheck) and report what you observed. The `scripts/` dir has smoke tests
(`smoke_e2e.sh`, `smoke_fork.sh`). Then a code review pass is warranted;
gap 5 (clone-safety) additionally warrants a security review.
