# Changelog

All notable changes to crucible are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and crucible aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches `v1.0` — until then, `0.x` releases may change behavior as the design
settles.

## [Unreleased]

### Added

- **Update a running app — `crucible app update <name>` / `PUT /apps/{name}`.**
  Replaces the app's spec (same flags as `create`; name immutable) and redeploys
  its instance — the daemon bumps the app's generation and the reconciler
  destroys the old instance and boots a fresh one from the new spec. Desired
  running/stopped is retained. Also on the Go SDK (`UpdateApp`) and the MCP
  `update_app` tool (→ 20 tools).
- **Seed app health from the image's `HEALTHCHECK`.** An app that declares no
  health of its own now inherits the image's Docker `HEALTHCHECK` when present —
  derived as an `exec` check at first boot and persisted. `--health`/`--health-cmd`
  still override.

## [0.4.1] — 2026-07-12

Apps you can actually deploy: real config, real egress. Everything here builds on
v0.4.0 durable apps and applies across `app create`, `run`, and `sandbox create`.

### Added

- **Real egress for trusted workloads (A6).** Two opt-in modes widen egress past
  the hostname allowlist, on `app create`, `run`, and `sandbox create`:
  `--net-full-egress` (reach any public host) and `--net-allow-cidr
  203.0.113.0/24` (reach IP literals in a public prefix). The invariant is
  **public unicast only** — metadata/link-local (`169.254.169.254`), RFC1918,
  CGNAT, loopback, and the reserved ranges are always dropped; a wholly-private
  CIDR reaches nothing. The nft drop list is unit-tested to agree with the DNS
  proxy's `IsPublicUnicast` guard so the two can't drift. Gated by a new
  `net_full_egress` scoped-token policy grant (default off, so a `net_allow_max`
  hostname ceiling can't be bypassed) and the MCP server's `--net-allow-max`
  guardrail. Also on the MCP `create_app`/`create_sandbox`/`run` tools.

- **Exec health checks — `crucible app create --health-cmd '<command>'`.** The
  daemon runs the command in the guest over vsock (exit 0 = healthy), joining the
  existing `http`/`tcp` probes; works even for an app with no network. Also on the
  MCP `create_app` tool (`health_type: "exec"` + `health_cmd`). (Auto-seeding a
  check from an image's own Docker `HEALTHCHECK` remains a follow-up.)

- **App env config — `crucible app create -e/--env KEY=VALUE` (repeatable).**
  Environment variables are delivered to the app's entrypoint (image `ENV` <
  your `--env`, so yours win). Surfaced on the CLI and the MCP `create_app` tool
  (`env`); the daemon already carried the field.
- **Publish an image's declared ports — `-P`/`--publish-all`.** On `app create`,
  `run`, and `sandbox create`: publishes every port the image `EXPOSE`s, each to
  the same host port number (guest N → host N — deterministic, unlike docker's
  random-host-port `-P`). tcp only; an explicit `-p` for a guest port wins. Also
  on the MCP `create_app` / `create_sandbox` tools (`publish_all`).

## [0.4.0] — 2026-07-11

### Added

- **Durable apps (v0.4) — workloads that survive a daemon restart and
  self-heal.** `crucible app create <name> --image … -p H:G --restart always
  --health http:PORT[:PATH]` promotes a workload to a named app the daemon keeps
  a healthy instance of: it restarts the instance on failure with exponential
  backoff and a crash-loop guard, health-checks it (http/tcp) and restarts on
  sustained failure, and — the headline — **re-creates it from persisted desired
  state after a daemon restart or host reboot** (a bbolt control-plane store +
  a reconcile loop; the ephemeral `sandbox` primitive is unchanged). Full
  surface: `app ls|get|rm|logs|exec|shell`, REST `/apps`, Go SDK (`CreateApp`/
  `ListApps`/`GetApp`/`DeleteApp` + an `App` handle), and four MCP tools
  (`create_app`/`list_apps`/`get_app`/`delete_app`), bringing the MCP surface to
  **19 tools**. Durability is desired-state reconcile (re-create), not live-VM
  re-attach — see the updated threat-model INV-6.

## [0.3.4] — 2026-07-11

### Added

- **`crucible fork -p HOST:GUEST`** — publish a host port on a fork (`docker
  run -p` semantics for copies). The fork API accepts an optional JSON body
  (`{"count", "publish"}`); publishing requires count 1 since host ports are
  exclusive. Forwarders roll back with the fork, close on delete, and appear
  in the fork's `published` field. Go SDK: `Fork(ctx, id, count, publish...)`
  (variadic, source-compatible).

## [0.3.3] — 2026-07-11

**The SDK foundation release.** The typed client and wire types are now a
public, dependency-free Go module (`github.com/gnana997/crucible/sdk`,
versioned independently as `sdk/vX.Y.Z`), the whole REST contract fans out
from one drift-guarded OpenAPI spec into generated TypeScript and Python
types, interactive exec gained a WebSocket transport any language can speak,
and the binary frame protocol is now specified and fixture-tested so SDKs in
new languages can be built — and verified — without a daemon or KVM.

### Added

- **Interactive exec over WebSocket** — `GET /sandboxes/{id}/exec` + upgrade:
  the cross-language transport for full-duplex exec (fetch-style HTTP stacks
  can't speak the hijacked `?stdin=1` stream; WebSocket also traverses L7
  proxies). First message is the JSON `ExecRequest`; after that the binary
  message payloads carry the exact same frame stream as the hijacked path.
  Gated as `exec` (not `read`) under scoped tokens. See [docs/wire.md](docs/wire.md).
- **Public Go SDK** — `internal/client` + `internal/api` + the shared wire
  types promoted to a nested, **zero-dependency** Go module: package
  `crucible` (client + `Sandbox`/`Snapshot` handles), `sdk/api` (REST DTOs),
  `sdk/wire` (frame codec + exec/service/files shapes). Typed errors
  (`ErrNotFound`/`ErrUnauthorized`/`ErrPolicyDenied` over structured
  `*Error`), pagination-ready `Page[T]` lists, SDK-owned `Identity`. The CLI,
  TUI, and MCP server now run on the same public package.
- **`docs/wire.md`** — the language-neutral wire spec: frame layout, chunking
  and termination rules, both interactive transports, files/tar semantics,
  and a four-step conformance recipe for SDK authors.
- **Conformance fixtures** (`sdks/fixtures/`) — recorded frame streams +
  manifest, generated from the real codec, so an SDK codec in any language is
  testable with no daemon and no KVM. Guarded by CI drift checks.
- **Generated TS + Python types** — `make gen` fans the OpenAPI spec out to
  `sdks/ts/src/schema.gen.ts` (openapi-typescript) and
  `sdks/python/crucible/models.py` (Pydantic v2 via datamodel-code-generator),
  pinned versions, CI `codegen-drift` job.
- **TypeScript SDK scaffold** (`sdks/ts`) — zero-runtime-dependency fetch
  client with streaming exec, typed errors, and a hand-written frame codec
  passing the full conformance suite; not yet published to npm.
- **`smoke_ws_exec.sh`** + `scripts/wsexec` — real-KVM smoke for the
  WebSocket transport, driving it exactly as a non-Go SDK would.

### Changed

- OpenAPI schema components renamed `Agentwire*` → `Wire*` (internal package
  name no longer leaks into the public spec).
- `internal/agentwire` now holds only the private daemon↔guest protocol
  (identity refresh, static network config, vsock port); the client-visible
  wire contract lives in the public `sdk/wire`.

## [0.3.2] — 2026-07-09

**The "drop your code in and run it" release.** Push local files straight into a
running sandbox with no image build and no Dockerfile, read bounded file content
back, watch a sandbox's logs live in the TUI, and stand the whole thing up on
Linux with a single command that provisions everything (including the guest
kernel) and starts the service. The launch line: it's the safe `docker run` for
untrusted or agent-written code, now with a first-class way to get code *in*.

### Added

- **`crucible cp <src> <id>:<dest>`** — push a local file or directory into a
  running sandbox as a tar stream over HTTP; the guest agent extracts it into
  the sandbox rootfs, rejecting path escapes (`..`, absolute paths, and symlinks
  that would leave the destination). No image build, no Dockerfile.
- **MCP `write_files` and `read_file`.** `write_files` drops content into a
  sandbox (exec-gated); `read_file` reads **bounded** file content back (UTF-8,
  base64 for binary, with a `truncated` flag), read-gated. crucible deliberately
  returns file content only and does not extract archives onto the host. Brings
  the MCP surface to **15 tools**. See [docs/mcp.md](docs/mcp.md).
- **TUI live logs view** (`l`): tail a sandbox's **durable** logs (service output
  + exec activity) live in a scrolling pane, each line timestamped with `stderr`
  highlighted, auto-following the tail unless you scroll up. The "docker + k9s"
  view; because the logs are durable they survive the sandbox itself.
- **One-command Linux install.** `install.sh --with-deps --enable` now provisions
  the **guest kernel** too (mirrored, pinned, and checksum-verified, with a
  firecracker-CI fallback), so `curl … | sudo bash -s -- --with-deps --enable`
  boots a working daemon end to end with nothing left to do by hand.
- **Mirrored guest kernel as a release asset.** The pinned `vmlinux-x86_64`
  (plus its `.sha256`) is published with each release for supply-chain
  independence from upstream object storage.
- Demos (`demo/*.tape` + GIFs) for `build`, `run`, `cp`, and a combined
  build → run → curl → TUI → logs hero; docs for `crucible cp`, `crucible shell`,
  the MCP file tools, and the TUI logs view. Smoke: `scripts/smoke_cp.sh`.

### Changed

- **`crucible run` long-lived hints** are aligned into a readable block instead
  of one run-on line, matching the CLI's other output.
- **Install robustness:** the rerun one-liner is correct when piped
  (`curl | sudo bash`), checkout-vs-download detection ensures a reinstall runs
  the release binary rather than a local dev build, and egress auto-wiring keys
  off the actual `CRUCIBLE_FLAGS=` line rather than matching template comments.

## [0.3.1] — 2026-07-09

**Cross-platform client + frictionless install.** The CLI, TUI, and MCP server
are thin HTTP clients over the daemon's REST API, so they now build and ship for
macOS and Windows and drive a remote Linux daemon. A reworked installer turns
setup into a one-liner for both the full daemon and a client-only install.

### Added

- **Cross-platform client binaries.** macOS and Windows client artifacts are
  built in CI and published with each release; the daemon itself stays
  Linux-only (it needs KVM + Firecracker).
- **`install.sh --client`** — install just the client (no root, no VM), pointed
  at a remote daemon with `--addr`/`--token`, with client-mode detection and
  interactive prompts for a smooth first run.
- **Release acceptance smokes** — an end-to-end acceptance smoke plus a full
  smoke runner for validating a release build.

### Changed

- **Installer hardening:** cross-platform install instructions, a warning when
  `e2fsprogs` tools are missing, and an updated config example for OCI images.
- Internal: the egress allowlist moved to a leaf package so the client
  cross-compiles without pulling in Linux-only daemon code.

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

[0.3.2]: https://github.com/gnana997/crucible/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/gnana997/crucible/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/gnana997/crucible/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gnana997/crucible/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/gnana997/crucible/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/gnana997/crucible/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gnana997/crucible/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gnana997/crucible/releases/tag/v0.1.0
