# Changelog

All notable changes to crucible are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and crucible aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches `v1.0` — until then, `0.x` releases may change behavior as the design
settles.

## [0.3.0] — 2026-07-09

**The "safe `docker run` for untrusted code" release.** Boot an unmodified OCI
image in a Firecracker microVM in one command, poke at it with a real
interactive shell, and tear it down — with default-deny egress and
fork-for-parallel-exploration throughout. The launch line: the moment you'd
reach for `docker run` on code you don't fully trust (a random repo, something
an agent just wrote), reach for `crucible run` instead.

> **Ephemeral contract.** v0.3.0 sandboxes are *consciously ephemeral* — a
> daemon restart does **not** resurrect running sandboxes (their registry
> records and durable logs persist; the live VMs do not). That is the right
> contract for "run a sketchy repo, test it, tear it down." Durable,
> self-healing long-lived workloads are v0.4.

### Added

- **Interactive shell — `crucible shell <id>`** (and `crucible sandbox exec -i`).
  A real, long-lived `/bin/sh` into a running sandbox over a hijacked
  full-duplex vsock stream: `cd`/env persist across commands and stdin
  round-trips at interactive latency. Line-buffered, **no PTY** (full-screen
  programs, colors, and Ctrl-C job control are v0.4). Adds `FrameStdin` /
  `FrameStdinClose` to the agent wire; the one-shot `/exec` path is untouched.
- **TUI session view.** The detail view keeps a **scrollback** of every command
  in a session, and **`tab`** opens the interactive shell (from the list, or
  inside detail) so commands share state — riding the same `ExecInteractive`.
- **`crucible run <image>`** — the docker-parity headline: acquire → boot the
  image's entrypoint → publish ports (`-p`) → **long-lived by default**. `--rm`
  tails logs in the foreground and removes the sandbox on Ctrl-C. The previous
  throwaway-command shape stays as `crucible run -- <command>`.
- **`crucible build [-t tag] [-f Dockerfile] <context>`** — `docker build`
  locally, then load the result into crucible's image store in one verb (Docker
  stays client-side; the daemon is Docker-free). Prints the converted digest,
  ready for `crucible run`.
- **`crucible stop <id>` / `crucible rm <id>`** — top-level ops verbs: graceful
  stop (image StopSignal + grace, the sandbox remains) and hard remove.
- **`--disk <size>`** on `sandbox create` / `run` — grow the writable rootfs
  (e.g. `2G`, `512M`) by resizing the *per-sandbox clone* (`resize2fs`) before
  boot; the shared image/profile ext4 is never touched.
- **MCP surface for the wedge.** `create_sandbox` / `run` gain `image` + `pull`,
  `disk_mib`, and (create) `publish`; two new tools — **`logs`** (durable
  service/exec logs that survive the sandbox) and **`stop_sandbox`** (graceful
  stop). See [docs/mcp.md](docs/mcp.md).
- Smokes: `scripts/smoke_shell.sh`, `smoke_reap.sh`, `smoke_build_run.sh`.

### Changed

- **`crucible run` is dual-mode**, selected by the `--` separator: a bare
  positional is an *image* (`run nginx -p 8080:80`); `-- <cmd>` is a *throwaway
  command* (the prior behavior, unchanged).
- **Ctrl-C is graceful across the CLI.** Client commands cancel their context on
  SIGINT/SIGTERM instead of being hard-killed, so `run --rm` cleans up, `logs
  -f` stops cleanly, and an interactive `shell`/`exec -i` tears the guest
  process down on exit.

### Fixed

- **Bare commands resolve on OCI images.** The PID-1 (init-mode) agent spawns via
  `os.StartProcess`, which does no `PATH` search — so `sh` / `sh -c` (the TUI,
  `exec -- sh`, `shell`) failed to start on image sandboxes with `exit -1`. The
  init exec path now resolves `argv[0]` against the child's `PATH`, matching
  profile mode and Docker.
- **Orphan reaping is complete.** A killed daemon leaves no lingering
  firecracker: startup now sweeps live orphan **processes** (scoped by
  `--chroot-base-dir`) alongside the existing chroot-driven reap, and mops up
  empty orphan **cgroup directories** whose chroot is already gone (these
  previously accumulated indefinitely).

### Security

- **Scope stated honestly.** v0.3.0 supports *running code you distrust on your
  own host*; it does **not** yet support hosting mutually-distrusting tenants on
  one host — that is gated on a hardening + external-audit pass. The microVM +
  jailer + default-deny-egress boundary is described precisely in
  [SECURITY.md](SECURITY.md).
- MCP image / publish / disk params pass through under the **existing** operator
  guardrails (timeout / net-allow / fork / sandbox clamps) — no new
  agent-widenable capability, and the resolved-IP range filter still gates all
  egress.

## [0.2.0] — 2026-07-08

The TUI release: a live terminal control center for a crucible daemon — see
running sandboxes, the fork tree, and interactive streaming `exec` at a glance,
and drive create / snapshot / fork / delete without leaving the dashboard. Like
the CLI and MCP server it's a thin consumer of the same typed client
(`internal/client`), so a dashboard action and a CLI command hit the exact same
API path and can't drift.

### Added

- **`crucible tui`** — a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
  dashboard that polls the daemon and renders live: a sandbox table (id, profile,
  age, CPU/memory, network, fork mark), a **fork-tree view** (`t`) built from the
  sandbox + snapshot genealogy, and a **detail + exec view** (`enter`) with a
  scrolling viewport where a command's stdout/stderr stream live and finish with
  an exit chip. Connects with the usual `--addr`/`--token`/`--tls-skip-verify`;
  the header shows the token's scope when the daemon reports one. See
  [docs/tui.md](docs/tui.md).
- **Actions with scope-gating.** Create (`c`), snapshot (`s`), fork (`f`, from
  the selected sandbox's latest snapshot), and delete (`d`, with a `y`/`n`
  confirm) run as async calls with status-line feedback. Each is gated on the
  token's policy — forbidden operations are struck through in the hint and
  rejected on keypress, mirroring what the daemon enforces authoritatively. The
  layout is responsive: hints compact on a narrow terminal and nothing overflows.
- **Fork lineage on the API.** `SandboxResponse` gains `source_snapshot_id` (the
  snapshot a sandbox was forked from, stamped in `forkOne`), so the fork
  genealogy is reconstructable by any client — this is what the tree view draws.
- **`crucible daemon --max-fork`** (env `CRUCIBLE_MAX_FORK`) — bound how many
  sandboxes a single fork request may create (0 = the built-in default of 64). A
  scoped token's own `max_fork` can still only tighten it.
- **Benchmark harness** — `cmd/crucible-bench` (`make bench`) drives a real
  daemon through `internal/client` and reports latency distributions, fork
  fan-out scaling, lazy-memory efficiency, and density; see
  [docs/benchmarks.md](docs/benchmarks.md).
- `docs/tui.md` and `demo/tui.tape`, `demo/network.tape`, `demo/bench.tape`
  [vhs](https://github.com/charmbracelet/vhs) scripts for regenerating the demo
  GIFs.

### Changed

- `VISION.md` observability wording is now precise: per-exec results (with
  CPU/memory/I/O usage) are **returned to the caller, not yet persisted** —
  durable per-sandbox activity logs are called out as the next step rather than
  implied to ship today.

## [0.1.3] — 2026-07-07

Scoped / policy tokens: bind an API key to a policy the **daemon** enforces, so a
leaked or handed-out key is worthless beyond its policy — the fix that makes the
MCP guardrails a real boundary and remote/hosted access genuinely safe.

### Added

- **Scoped tokens.** `crucible daemon token add --policy <file> --ttl <dur>`
  mints a key bound to a JSON policy — allowed operations, an egress ceiling, a
  profile allowlist, and resource caps (sandboxes / fork / timeout / vCPU /
  memory) — with an optional expiry. The daemon enforces the policy on every
  request; an unscoped key keeps full access, so nothing regresses. See
  [docs/policy.md](docs/policy.md).
- **`crucible policy validate <file|->`** — the *same* validation that gates
  `token add` (fail-closed), reporting every problem at once.
- **`crucible policy show` / `GET /whoami`** — the effective policy for the
  presenting token, so a client can discover exactly what it may do.
- **MCP tool mirror.** `mcp serve` asks `/whoami` at startup and advertises only
  the tools the token's policy permits (the daemon still enforces regardless).
- `docs/policy.md`, plus a scoped-token check in `scripts/smoke_mcp.sh`.

### Changed

- `daemon token list` now shows each key's scope (full/scoped) and expiry.
- The MCP server's `--max-*` / `--net-allow-max` flags are **narrow-only**: they
  layer on top of the daemon-enforced token policy and can only tighten it.

### Security

- **The local same-user bypass is closed for scoped tokens.** Enforcement is
  daemon-authoritative, so an agent that steals its MCP server's scoped key and
  calls the loopback daemon directly gets only the bounded capability it already
  had. Expired tokens are rejected (`401`), and `max_sandboxes` is counted
  per-token so tokens don't share a budget. (OAuth-style short-lived session
  tokens — an MCP server exchanging its key for a scoped session token — are
  deferred.)

## [0.1.2] — 2026-07-07

The MCP release: drive crucible from any MCP agent as native tools, and reach a
remote/hosted daemon safely with API-key auth.

### Added

- **MCP server — `crucible mcp serve`.** A stdio [Model Context
  Protocol](https://modelcontextprotocol.io) server so any MCP agent (Claude
  Code, Cursor, …) can drive crucible as native tools. It is a thin client of
  the daemon (built on the official Go MCP SDK), so an MCP tool call and the
  equivalent CLI command hit the identical code path and can't drift. Full tool
  catalog: `run` (create → exec → delete in one call), `create_sandbox`,
  `exec`, `snapshot`, `fork`, `list_sandboxes`, `inspect_sandbox`,
  `delete_sandbox`, `list_snapshots`, `delete_snapshot`, `list_profiles`.
  Because the server is a client, `--addr` can point at a remote daemon — the
  same local stdio subprocess bridges to it. See [docs/mcp.md](docs/mcp.md).
- **MCP operator guardrails.** The operator fixes policy at launch; the agent
  can never widen it. Network stays default-deny; `--net-allow-max` caps
  agent-chosen egress; `--max-sandboxes` (8), `--max-fork` (8), and
  `--max-timeout` (300s) bound resources; `--default-profile` /
  `--allow-profiles` and `--tools` / `--deny-tools` reduce the surface (a
  filtered tool never appears in `tools/list`).
- **Daemon API-key authentication.** Bearer keys on the docker/kubectl model:
  `crucible daemon token add` / `list` / `revoke` (rotate = add + revoke). Keys
  are stored as SHA-256 hashes, so a leaked token file yields no usable keys.
  The daemon checks `Authorization: Bearer <key>` on every request (`/healthz`
  exempt); auth turns on automatically once any key exists. Clients pass a key
  with `--token` or `CRUCIBLE_TOKEN`.
- **TLS for the daemon** — `--tls-cert` / `--tls-key`, with `--tls-skip-verify`
  on the client for self-signed dev certs.
- **`scripts/smoke_mcp.sh`** — integration smoke that drives `crucible mcp
  serve` against a real daemon (tool catalog, `run` round-trip, a guardrail
  rejection, the auth path) and guards against VM/cgroup leaks.
- Docs: [docs/mcp.md](docs/mcp.md), an MCP threat-model section in
  [SECURITY.md](SECURITY.md), and auth/MCP notes across
  [docs/api.md](docs/api.md), [docs/cli.md](docs/cli.md), and
  `packaging/crucible.env.example`.

### Changed

- **`--jail-gid` now defaults to the `kvm` group** so the jailed firecracker
  can open `/dev/kvm` out of the box (it reaches the device through its group).
  Falls back to `10000` on hosts without a kvm group; an explicit `--jail-gid`
  still wins. This removes a cryptic first-run `creating KVM object: Permission
  denied` in jailer mode.

### Fixed

- **VM leak on jailer-mode delete.** A sandbox delete (and `run`/fork cleanup)
  signalled only the jailer process; with `--new-pid-ns` the firecracker child
  lives in its own PID namespace and was orphaned to init — a leaked microVM
  that also kept its cgroup populated (the `jailer cleanup failed … device or
  resource busy` warning). Teardown now kills the whole VM process set (jailer
  **and** firecracker, by their `--id` cmdline token) and waits for it to drain
  before removing the chroot and cgroup. The cgroup `rmdir` additionally
  retries the brief post-exit window during which cgroupfs still reports the
  group populated.

### Security

- Daemon API-key auth (above) provides remote/hosted access control and
  per-client revocation. Binding a **non-loopback** address is refused unless
  both API keys and TLS are configured — a bearer token over plaintext HTTP is
  a leaked token.
- MCP guardrails bound what a (potentially prompt-injected) agent can do; the
  daemon's default-deny networking and resolved-IP range filter still apply
  regardless of an agent-chosen allowlist, so agent egress can never reach
  cloud-metadata or internal addresses.
- **Known limitation:** a local, same-OS-user agent with a shell tool can read
  the daemon key and bypass the MCP guardrails by calling the loopback daemon
  directly. Bearer keys make the *remote* story solid but don't bound this; the
  planned fix is daemon-enforced **scoped tokens** (deferred). See
  [docs/mcp.md#limitations](docs/mcp.md#limitations).

## [0.1.1] — 2026-07-07

### Fixed

- Guest networking on the native rootfs profiles: install `udev` + `dbus` and
  write `/etc/resolv.conf` so DNS resolves inside the guest.
- Installer: keep the daemon's work-base persistent (off `/tmp`, so the
  registry survives reboot), quiet the installer output, and complete the
  install docs.

## [0.1.0] — 2026-07-07

Initial release — the core single-host Firecracker microVM sandbox runtime.

### Added

- Firecracker microVM runtime with **jailer isolation** (per-VM chroot + mount/
  PID namespaces + privilege drop) and host cgroup v2 quotas.
- **Snapshot / fork** with lazy guest memory via `userfaultfd` (fork serves
  page faults from the snapshot's memory file instead of byte-copying RAM).
- **Clone-safety**: each fork's kernel RNG is reseeded and machine identifiers
  rotated before it can be exec'd, so no two forks wake sharing RNG/UUIDs.
- **Per-sandbox networking**, default-deny: own netns/veth/tap, per-netns DHCP,
  a DNS proxy that only resolves allowlisted hostnames, and a resolved-IP range
  filter (blocks link-local / RFC1918 / CGNAT, closing SSRF to cloud metadata).
- **Durable registry + reconcile-on-restart**: sandbox/snapshot records are
  journaled; on startup snapshots are re-adopted and orphaned state is reaped.
- CLI over the REST API (`sandbox`, `snapshot`, `fork`, `profile`, `run`;
  `-o json`), native language rootfs profiles (base/python/node/go), a
  Prometheus `/metrics` endpoint, and an install script + systemd unit.

[0.3.0]: https://github.com/gnana997/crucible/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gnana997/crucible/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/gnana997/crucible/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/gnana997/crucible/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gnana997/crucible/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gnana997/crucible/releases/tag/v0.1.0
