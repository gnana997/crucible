---
title: Install
description: "Install crucible with one command: the Linux daemon (systemd service) or the cross-platform client (macOS / Windows / Linux) that drives a remote daemon."
---

# Install

crucible ships as a **single binary** that is both the daemon and the CLI. One
installer, `install.sh`, handles both roles and picks the right one for your OS:

- **Linux** → installs the **daemon** as a systemd service (plus the CLI). The
  daemon needs KVM + Firecracker, so it only runs on Linux.
- **macOS / Windows** → installs the **client** only (the CLI + `crucible mcp
  serve`). Everything but the daemon is a thin HTTP client, so it drives a
  *remote* Linux daemon over the network.

```bash
# Linux daemon (needs root + systemd) — install, fetch deps, start:
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo sh -s -- --enable --with-deps

# macOS / Windows / Linux client — point it at a Linux daemon:
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh \
  | sh -s -- --client --addr https://my-linux-host:7878 --token <key>
```

A default install **mints no key and leaves auth off** — the local CLI on the
daemon host just works. You only turn on auth when you open the daemon to other
machines (see [Remote access](#remote-access)).

> [!NOTE]
> Running the daemon *locally* on a Mac would mean a nested Linux VM, and nested
> virtualization on Apple Silicon is unreliable — so the supported macOS/Windows
> path is a **remote Linux daemon + local client**, not a workaround. See the
> [Vision FAQ](VISION.md) for the reasoning.

## Modes at a glance

| Your OS | Installed by default | `--client` forces | Needs root | Needs systemd |
|---|---|---|---|---|
| Linux (x86_64) | **Daemon** + CLI | client only | yes (daemon) | yes (daemon) |
| macOS (arm64/amd64) | **Client** | — (already client) | no | no |
| Windows (amd64) | **Client** | — (already client) | no | no |

`--client` forces client-only mode **anywhere** — e.g. a Linux box that should
only drive a remote daemon, not run one.

---

## What the client install gives you (macOS / Windows / Linux)

Run with `--client` (automatic on macOS/Windows). No root, no systemd, no daemon
— just the tools that talk to one:

- **The `crucible` binary** — the full CLI and `crucible mcp serve` (the MCP
  server for Claude Code / Cursor / any MCP client). On Windows it's
  `crucible.exe`.
- **Downloaded + checksum-verified** from the matching release asset:

  | Platform | Release asset |
  |---|---|
  | macOS arm64 | `crucible_<tag>_darwin_arm64.tar.gz` |
  | macOS amd64 | `crucible_<tag>_darwin_amd64.tar.gz` |
  | Windows amd64 | `crucible_<tag>_windows_amd64.zip` (needs `unzip`) |
  | Linux amd64 | `crucible_<tag>_linux_amd64.tar.gz` |

- **Installed without root.** Into `CLIENT_BINDIR` if set, else the system
  `bin` dir when it's writable (or you're root), else `~/.local/bin`. The
  installer tells you if that dir isn't on your `PATH` and how to add it.
- **A client config file** at `~/.config/crucible/env` (mode `600`) exporting
  `CRUCIBLE_ADDR` / `CRUCIBLE_TOKEN` when you pass `--addr` / `--token`. Source
  it from your shell rc, or `source ~/.config/crucible/env` on demand. The
  installer never edits your shell rc files for you.
- **Interactive prompts** for the daemon URL and key **only** when run from a
  real terminal without `--addr`/`--token`. A piped install (`curl | sh`) stays
  non-interactive and just prints guidance.

What it does **not** do: install Firecracker, a kernel, a rootfs, a systemd unit,
or any daemon. The client is useless on its own — it needs a Linux daemon to
point at. After installing, wire it into an agent:

```
command: crucible
args:    ["mcp", "serve"]
env:     CRUCIBLE_ADDR, CRUCIBLE_TOKEN
```

> [!NOTE]
> There is no prebuilt daemon for macOS/Windows and no prebuilt **client** for
> Windows arm64 or macOS via a package manager — the installer only fetches the
> assets in the table above. On an unsupported platform, build from source:
> `go build ./cmd/crucible`.

---

## What the daemon install does (Linux)

The Linux path (default, or forced off with `--client`) requires **root +
systemd**. It:

1. **Installs the binary** to `$PREFIX/bin/crucible` (default `/usr/local/bin`).
2. **Creates the state tree** under `$STATEDIR` (default `/var/lib/crucible`):
   `profiles/` (pre-baked `<profile>.ext4`), `images/` (converted OCI image
   cache), `logs/` (durable per-sandbox logs).
3. **Installs the systemd unit** at `$UNITDIR/crucible.service` (default
   `/etc/systemd/system`) and runs `systemctl daemon-reload`.
4. **Enables lazy fork** — writes `vm.unprivileged_userfaultfd=1` to
   `/etc/sysctl.d/99-crucible.conf` so the jailed, uid-dropped Firecracker can
   use `userfaultfd` for on-demand snapshot memory.
5. **Writes the config** `$CONFDIR/crucible.env` (default `/etc/crucible`) from
   the template — **never clobbering an existing one** — and folds in sensible
   defaults: the host's default-route NIC for `--network-egress-iface`, and the
   ingress proxy on `:7879`.
6. **Drops an example scoped policy** at `$CONFDIR/policies/example.json` (inert
   — grants nothing until you mint a key against it).
7. **Optionally fetches dependencies** with `--with-deps` (below) and
   **optionally starts** the service with `--enable`.

A daemon install **mints no key** and leaves the daemon on loopback
(`127.0.0.1:7878`). It's zero-surprise: nothing is exposed until you ask.

### `--with-deps`: fetched dependencies

Opt-in and **checksum-verified**; each piece is skipped if already present.
**x86_64 only** (that's where the prebuilt rootfs + kernel exist):

| Piece | Source | Destination |
|---|---|---|
| firecracker + jailer | firecracker-microvm releases (`FC_VERSION`) | `$PREFIX/bin` |
| rootfs profile | crucible release `<ROOTFS_PROFILE>.ext4` | `$STATEDIR/rootfs-<tag>.ext4` |
| guest kernel | crucible release `vmlinux-x86_64` (firecracker-CI fallback) | `$STATEDIR/vmlinux` |

The rootfs is **tag-stamped** (`rootfs-<tag>.ext4`) and the installer re-points
`--rootfs` at it — so a version upgrade actually picks up the new rootfs (with
its current baked guest agent) instead of silently keeping a stale one. A rootfs
you supplied yourself (a `--rootfs` path the installer doesn't own) is left
untouched. See [Upgrading](#upgrading).

Without `--with-deps` you provide these yourself at the paths the config expects:

```
firecracker : /usr/local/bin/firecracker   (and /usr/local/bin/jailer)
kernel      : /var/lib/crucible/vmlinux
rootfs      : /var/lib/crucible/rootfs.ext4
profiles    : /var/lib/crucible/profiles/<name>.ext4
```

### Host prerequisites

Booting OCI images (`crucible run nginx:alpine`, `crucible build`) and `--disk`
sizing shell out to **e2fsprogs** — `mkfs.ext4`, `fsck.ext4`, `debugfs`,
`resize2fs`. The installer warns (doesn't fail) if they're missing:

```bash
apt-get install -y e2fsprogs   # or: dnf install e2fsprogs
```

---

## CLI flags

| Flag | Role | Description |
|---|---|---|
| `--client` | any | Install just the CLI + `mcp serve`; no daemon/systemd/root. Automatic on non-Linux. |
| `--addr URL` | client | Default daemon address → `CRUCIBLE_ADDR` (e.g. `https://host:7878`). |
| `--token TOK` | client | API key → `CRUCIBLE_TOKEN`. |
| `--enable` | daemon | `systemctl enable --now` the service after install. |
| `--with-deps` | daemon | Also fetch firecracker + jailer, a rootfs, and a guest kernel (opt-in, checksum-verified). |
| `--no-egress-auto` | daemon | Don't auto-wire the host's egress NIC into a fresh config. |
| `--no-proxy` | daemon | Don't enable the ingress proxy (reach apps by name) by default. |
| `--upgrade-config` | daemon | Apply missing flags (`--image-dir`, `--log-dir`, `--app-db`, `--registry-store`, `--network-egress-iface`, proxy, app→app) to an **existing** config. |
| `--connect-token` | daemon | Mint a scoped token and print a ready-to-paste client one-liner + MCP config. |
| `--token-name NAME` | daemon | Name for `--connect-token`'s key (default `remote-client`). |
| `--version TAG` | any | Release tag to install (default: latest published release). |
| `--binary PATH` | any | Install this local binary instead of downloading a release. |
| `-h`, `--help` | any | Print the header help and exit. |

## Environment variables

Pass them **after `sudo`** (`sudo VAR=val bash …`) so root's environment
actually carries them — `VAR=val sudo bash` puts the var on `sudo`, which resets
the environment and drops it before the script runs (a common footgun with the
piped `curl | sudo bash` form).

| Variable | Default | Applies to | Description |
|---|---|---|---|
| `PREFIX` | `/usr/local` | both | Install prefix; the binary lands in `$PREFIX/bin`. |
| `CLIENT_BINDIR` | (auto) | client | Where the client binary is installed (else system bin, else `~/.local/bin`). |
| `UNITDIR` | `/etc/systemd/system` | daemon | systemd unit directory. |
| `CONFDIR` | `/etc/crucible` | daemon | Config + policies directory. |
| `STATEDIR` | `/var/lib/crucible` | daemon | State: rootfs, kernel, profiles, images, logs, tokens. |
| `FC_VERSION` | `v1.16.1` | daemon | Firecracker version fetched by `--with-deps`. |
| `ROOTFS_PROFILE` | `base` | daemon | Which profile rootfs `--with-deps` fetches. |
| `KERNEL_URL` | (release asset) | daemon | Override the guest-kernel URL (uncompressed vmlinux). |
| `KERNEL_SHA256` | (pinned) | daemon | Literal digest or `.sha256` URL to verify a custom kernel. |
| `PROXY_LISTEN` | `:7879` | daemon | Ingress-proxy HTTP listen. `:80` for a production ingress; `host:port` pins an interface. |
| `PROXY_TLS_LISTEN` | (off) | daemon | TLS SNI-passthrough listen (needs a TLS-serving guest), e.g. `:7880` or `:443`. |
| `PROXY_DOMAIN` | `apps.local` | daemon | Base domain for name routing (`<app>.<domain>`). |
| `INTERNAL_NET` | `0` (off) | daemon | Set `1` to opt into [app→app networking](apps.md#app-to-app-networking) (experimental). |
| `INTERNAL_PORT` | `80` | daemon | Override the app→app VIP port when `INTERNAL_NET=1`. |

### Common recipes

```bash
# Production ingress on standard ports (root binds :80/:443 with no extra caps):
sudo PROXY_LISTEN=:80 PROXY_TLS_LISTEN=:443 bash install.sh --enable --with-deps

# Turn the ingress proxy off entirely:
sudo bash install.sh --enable --with-deps --no-proxy

# Enable app→app networking in one line (no manual config edit):
sudo INTERNAL_NET=1 bash install.sh --enable --with-deps

# Same, piped from curl (note the env goes AFTER sudo, and `-s --` before flags):
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh \
  | sudo INTERNAL_NET=1 bash -s -- --enable --with-deps

# Pin a specific release, install to a custom prefix:
sudo PREFIX=/opt/crucible bash install.sh --enable --with-deps --version v0.5.2

# Install from a local build (repo checkout) without downloading:
make build && sudo ./install.sh --enable --binary ./crucible
```

---

## The config file

Daemon flags live in `$CONFDIR/crucible.env` as a single `CRUCIBLE_FLAGS="…"`
line, passed verbatim to `crucible daemon` (systemd splits it on whitespace).
Edit it and restart to apply:

```bash
sudo systemctl restart crucible
```

Run `crucible daemon --help` for the full flag list. The installer seeds the
paths and feature flags for you; hand-editing is only needed for advanced tuning
(TLS, custom listen address, resource defaults).

## Upgrading

Re-run the installer with a newer `--version` (or latest). Two things make an
upgrade safe:

- **Existing config is never clobbered.** Add `--upgrade-config` to fold in any
  flags a newer release introduced (it only *adds* missing flags, never rewrites
  your values), then restart.
- **The rootfs is tag-stamped.** With `--with-deps`, an upgrade fetches
  `rootfs-<newtag>.ext4` and re-points `--rootfs` at it, so the daemon boots the
  new rootfs (and its current guest agent) rather than a stale one. The previous
  rootfs is kept on disk for rollback.

> [!TIP]
> **OCI images** (`crucible run <image>`, image-backed apps) get their guest
> agent injected from the daemon binary, not the profile rootfs — so upgrading
> the binary refreshes them. If an image app misbehaves right after an upgrade,
> its converted copy in `--image-dir` may be cached from the old agent; re-pull
> to re-convert.

## Remote access

To drive the daemon from another machine, mint a scoped key — this turns on auth
for **every** client, including local ones. Let the installer do it and print
the exact client command + MCP block:

```bash
sudo ./install.sh --connect-token --token-name my-laptop
```

Then make the daemon reachable: set `--listen` to a routable address **and**
`--tls-cert` / `--tls-key` in `$CONFDIR/crucible.env`, and restart. Non-loopback
listeners require both a key and TLS. See [policy.md](policy.md) for scoped
tokens and [cli/daemon.md](cli/daemon.md) for `crucible daemon token`.

## Uninstall

```bash
sudo systemctl disable --now crucible
sudo rm /etc/systemd/system/crucible.service /etc/sysctl.d/99-crucible.conf
sudo rm /usr/local/bin/crucible            # + firecracker/jailer if --with-deps installed them
sudo rm -rf /etc/crucible /var/lib/crucible # config + all state (rootfs, images, logs, tokens)
sudo systemctl daemon-reload
```

The client: delete the binary from its install dir and `~/.config/crucible/`.
