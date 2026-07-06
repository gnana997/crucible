# Changelog

All notable changes to crucible are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and crucible aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches `v1.0` — until then, `0.x` releases may change behavior as the design
settles.

## [0.1.2] — unreleased

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

[0.1.2]: https://github.com/gnana997/crucible/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/gnana997/crucible/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gnana997/crucible/releases/tag/v0.1.0
