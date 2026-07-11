#!/usr/bin/env bash
# install.sh — install crucible: the daemon (Linux) or a cross-platform client.
#
# The daemon needs KVM + Firecracker, so it runs on Linux as a systemd service.
# The client (CLI + `crucible mcp serve`) runs anywhere and drives a *remote*
# Linux daemon — that's the macOS / Windows path.
#
# Daemon (Linux, needs root + systemd):
#   # download the release binary and install the service:
#   curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo sh
#   # from a checkout or extracted tarball, and also fetch firecracker+kernel+rootfs:
#   sudo ./install.sh --enable --with-deps
#
# Client (macOS / Windows / Linux, no root, no systemd):
#   curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh \
#     | sh -s -- --client --addr https://my-linux-host:7878 --token <key>
#
# The mode is auto-selected by OS: Linux installs the daemon (+ client); macOS
# and Windows install the client only. Force client mode anywhere with --client.
# A default install mints NO key and leaves auth off — the local CLI just works;
# use --connect-token only when you want to serve *other* machines.
#
# Flags:
#   --client            install just the CLI (+ `mcp serve`); no daemon/systemd/root (auto on non-Linux)
#   --addr URL          (client) default daemon address → CRUCIBLE_ADDR (e.g. https://host:7878)
#   --token TOK         (client) API key → CRUCIBLE_TOKEN
#   --enable            (daemon) enable + start the service now
#   --with-deps         (daemon) also fetch firecracker+jailer, a rootfs, and a guest kernel — opt-in, checksum-verified
#   --no-egress-auto    (daemon) don't auto-wire the host's egress NIC into a fresh config
#   --upgrade-config    (daemon) apply missing --image-dir / --log-dir / --app-db / --network-egress-iface to an existing config
#   --connect-token     (daemon) mint a scoped token and print a ready-to-paste MCP config + client one-liner
#   --token-name NAME   (daemon) name for --connect-token's key (default: remote-client)
#   --version TAG       release tag (default: latest; download mode)
#   --binary PATH       install this local binary instead of downloading
#
# The daemon path runs as root but never calls sudo itself. Env overrides:
#   PREFIX (/usr/local), CLIENT_BINDIR (client install dir), FC_VERSION,
#   ROOTFS_PROFILE (default: base), KERNEL_URL / KERNEL_SHA256 (override kernel).

set -euo pipefail

REPO="gnana997/crucible"

PREFIX="${PREFIX:-/usr/local}"
BINDIR="$PREFIX/bin"
UNITDIR="${UNITDIR:-/etc/systemd/system}"
CONFDIR="${CONFDIR:-/etc/crucible}"
STATEDIR="${STATEDIR:-/var/lib/crucible}"

# --with-deps pins (override via env). Firecracker v1.15+ is required.
FC_VERSION="${FC_VERSION:-v1.16.1}"
ROOTFS_PROFILE="${ROOTFS_PROFILE:-base}"
# Pinned guest kernel for x86_64. --with-deps fetches + verifies it so the daemon
# boots out of the box: it prefers our own release asset (vmlinux-x86_64) for
# supply-chain independence, and falls back to the firecracker-CI bucket when the
# resolved release predates that asset. The pinned digest verifies either source
# (byte-identical — our release job just mirrors this kernel). Override with
# KERNEL_URL (+ optional KERNEL_SHA256 — a literal digest or a .sha256 URL).
# NOTE: bumping the kernel means updating this digest AND release.yml's kernel job.
DEFAULT_KERNEL_FALLBACK_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/vmlinux-6.1.102"
DEFAULT_KERNEL_SHA256="cf42303c29e8c4a02798f357ba056c5567baf074aaed4eec78c997fb9df08cf9"

# ROOT is the checkout / extracted-tarball dir the script runs from, used to
# install the local binary + packaging without downloading. This is only valid
# when $0 is an actual install.sh file on disk; when piped (curl | sudo bash),
# $0 is "bash" and dirname resolves to the CWD — which must NOT be mistaken for
# a checkout (it would install whatever ./crucible happens to be there). In that
# case point ROOT nowhere so we always download the release.
if [[ -f "$0" && "$0" == *install.sh ]]; then
    ROOT="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo /nonexistent)"
else
    ROOT="/nonexistent"
fi
BINARY="$ROOT/crucible"
BINARY_SET=0
VERSION=""
ENABLE=0
CLIENT=0
CLIENT_ADDR=""
CLIENT_TOKEN=""
WITH_DEPS=0
UPGRADE_CONFIG=0
CONNECT_TOKEN=0
TOKEN_NAME="remote-client"
NO_EGRESS_AUTO=0
CLIENT_EXPLICIT=0

die()  { echo "install: $*" >&2; exit 1; }
warn() { echo "install: warning: $*" >&2; }
# kv prints an aligned "    label: value" detail line under a "==> step" header.
kv()   { printf '    %-9s%s\n' "$1:" "$2"; }
# flags_have reports whether the daemon's CRUCIBLE_FLAGS line already carries a
# flag. It matches ONLY the CRUCIBLE_FLAGS= assignment, not the template's
# descriptive comments (which mention --network-egress-iface / --image-dir / …
# and would otherwise fool a whole-file grep into skipping the flag).
flags_have() { grep '^CRUCIBLE_FLAGS=' "$2" 2>/dev/null | grep -q -- "$1"; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --client) CLIENT=1; CLIENT_EXPLICIT=1; shift ;;
        --addr) CLIENT_ADDR="${2:?--addr needs a URL}"; shift 2 ;;
        --token) CLIENT_TOKEN="${2:?--token needs a key}"; shift 2 ;;
        --enable) ENABLE=1; shift ;;
        --with-deps) WITH_DEPS=1; shift ;;
        --no-egress-auto) NO_EGRESS_AUTO=1; shift ;;
        --upgrade-config) UPGRADE_CONFIG=1; shift ;;
        --connect-token) CONNECT_TOKEN=1; shift ;;
        --token-name) TOKEN_NAME="${2:?--token-name needs a name}"; shift 2 ;;
        --version) VERSION="${2:?--version needs a tag}"; shift 2 ;;
        --binary) BINARY="${2:?--binary needs a path}"; BINARY_SET=1; shift 2 ;;
        -h|--help) sed -n '2,38p' "$0"; exit 0 ;;
        *) die "unknown argument: $1 (see --help)" ;;
    esac
done

# Auto-select the mode by OS: the daemon is Linux-only, so on macOS/Windows we
# install the client. --client forces it anywhere (e.g. a Linux box that only
# drives a remote daemon).
if [[ "$CLIENT" -eq 0 && "$(uname -s)" != "Linux" ]]; then
    CLIENT=1
    echo "install: $(uname -s) detected — installing the client (the daemon is Linux-only)." >&2
fi

# RERUN is how the message hints tell the user to invoke this installer again.
# A piped install (curl | sudo bash) has $0 = "bash" / an fd path — not runnable
# — so fall back to the canonical one-liner; a file invocation uses the path.
case "$0" in
    */install.sh|install.sh|./install.sh) RERUN="sudo $0" ;;
    *) RERUN="curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo bash -s --" ;;
esac

# detect_platform sets OS (linux|darwin|windows) and ARCH (amd64|arm64).
detect_platform() {
    local u m
    u="$(uname -s)"; m="$(uname -m)"
    case "$u" in
        Linux)  OS=linux ;;
        Darwin) OS=darwin ;;
        MINGW*|MSYS*|CYGWIN*) OS=windows ;;
        *) die "unsupported OS: $u (build from source: go build ./cmd/crucible)" ;;
    esac
    case "$m" in
        x86_64|amd64)  ARCH=amd64 ;;
        arm64|aarch64) ARCH=arm64 ;;
        *) die "unsupported arch: $m" ;;
    esac
}

# resolve_tag prints the release tag to install (--version, or the latest).
resolve_tag() {
    local tag="$VERSION"
    if [[ -z "$tag" ]]; then
        command -v curl >/dev/null || die "curl is required to resolve the latest release"
        local api
        api=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest") \
            || die "could not query the latest release for $REPO (pass --version)"
        tag=$(printf '%s\n' "$api" | grep -m1 '"tag_name"' | cut -d'"' -f4)
        [[ -n "$tag" ]] || die "no published release found for $REPO (pass --version)"
    fi
    printf '%s' "$tag"
}

# fetch <url> <dest> — download a file, failing loudly.
fetch() { curl -fSL "$1" -o "$2" || die "download failed: $1"; }

# verify_sha256 <file> <sha256-url> — compare the file's digest to the first
# field of the checksum file at the URL. Works for both firecracker's
# ".sha256.txt" sidecars and crucible's own ".sha256" files (both start with
# the hex digest). A missing checksum URL is a hard failure — we never install
# an unverified dependency.
verify_sha256() {
    local file="$1" ref="$2" want got
    if [[ "$ref" =~ ^[0-9a-fA-F]{64}$ ]]; then
        want="$ref"                                   # a literal digest
    else
        want=$(curl -fsSL "$ref" 2>/dev/null | awk 'NF{print $1; exit}') \
            || die "could not fetch checksum: $ref"   # a URL to a checksum file
    fi
    [[ -n "$want" ]] || die "empty checksum for $(basename "$file")"
    got=$(sha256sum "$file" | awk '{print $1}')
    [[ "$want" == "$got" ]] || die "checksum mismatch for $(basename "$file") (want $want, got $got)"
}

# ============================================================================
# Client install (macOS / Windows / Linux) — no root, no systemd.
# ============================================================================
install_client() {
    detect_platform

    # When run from a real terminal (not `curl | sh`), ask for the daemon URL
    # and key if they weren't passed — a client is only useful pointed at a
    # daemon. Piped installs stay non-interactive and fall back to guidance.
    if [[ -t 0 ]]; then
        if [[ -z "$CLIENT_ADDR" ]]; then
            printf 'Daemon URL (e.g. https://my-linux-host:7878) [skip]: ' >&2
            read -r CLIENT_ADDR || true
        fi
        if [[ -n "$CLIENT_ADDR" && -z "$CLIENT_TOKEN" ]]; then
            printf 'API key for %s [skip]: ' "$CLIENT_ADDR" >&2
            read -r CLIENT_TOKEN || true
        fi
    fi

    # Pick an install dir that doesn't need root. Honor CLIENT_BINDIR, else use
    # the system bindir when writable, else ~/.local/bin.
    local bindir="${CLIENT_BINDIR:-}"
    if [[ -z "$bindir" ]]; then
        if [[ -w "$BINDIR" || "$(id -u)" -eq 0 ]]; then bindir="$BINDIR"; else bindir="$HOME/.local/bin"; fi
    fi
    mkdir -p "$bindir"

    local src="" tmp=""
    if [[ "$BINARY_SET" -eq 1 ]]; then
        [[ -x "$BINARY" ]] || die "--binary $BINARY is not executable"
        src="$BINARY"
    else
        command -v curl >/dev/null || die "curl is required to download the client"
        command -v tar  >/dev/null || die "tar is required to download the client"
        local tag asset kind
        tag=$(resolve_tag)
        case "$OS/$ARCH" in
            darwin/arm64|darwin/amd64) asset="crucible_${tag}_${OS}_${ARCH}.tar.gz"; kind=tar ;;
            linux/amd64)               asset="crucible_${tag}_linux_amd64.tar.gz";   kind=tar ;;
            windows/amd64)             asset="crucible_${tag}_windows_amd64.zip";     kind=zip ;;
            *) die "no prebuilt client for $OS/$ARCH yet — build from source: go build ./cmd/crucible" ;;
        esac
        local url="https://github.com/$REPO/releases/download/$tag/$asset"
        tmp="$(mktemp -d)"
        echo "==> downloading crucible client $tag ($OS/$ARCH)"
        fetch "$url" "$tmp/$asset"
        verify_sha256 "$tmp/$asset" "${url}.sha256"
        echo "    checksum verified"
        if [[ "$kind" == zip ]]; then
            command -v unzip >/dev/null || die "unzip is required to extract $asset"
            unzip -q "$tmp/$asset" -d "$tmp/x"
        else
            mkdir -p "$tmp/x" && tar -C "$tmp/x" -xzf "$tmp/$asset"
        fi
        # Find the crucible binary inside the single top-level dir.
        src="$(find "$tmp/x" -type f \( -name crucible -o -name crucible.exe \) | head -1)"
        [[ -n "$src" ]] || die "no crucible binary found inside $asset"
    fi

    local out="$bindir/crucible"
    [[ "$OS" == windows ]] && out="$bindir/crucible.exe"
    cp "$src" "$out"
    chmod 755 "$out"
    [[ -n "$tmp" ]] && rm -rf "$tmp"
    echo "==> Installed crucible client"
    kv binary "$out"
    kv version "$("$out" version 2>/dev/null || echo unknown)"

    # Write a client env file the CLI + `mcp serve` pick up (CRUCIBLE_ADDR /
    # CRUCIBLE_TOKEN). Non-invasive: we don't touch shell rc files.
    local envfile="${HOME}/.config/crucible/env"
    mkdir -p "$(dirname "$envfile")"
    {
        echo "# crucible client config — written by install.sh --client"
        [[ -n "$CLIENT_ADDR" ]]  && echo "export CRUCIBLE_ADDR=\"$CLIENT_ADDR\""
        [[ -n "$CLIENT_TOKEN" ]] && echo "export CRUCIBLE_TOKEN=\"$CLIENT_TOKEN\""
    } > "$envfile"
    chmod 600 "$envfile"

    echo
    echo "==> Next steps"
    echo
    case ":$PATH:" in
        *":$bindir:"*) : ;;
        *) echo "    Add $bindir to your PATH:"
           echo
           echo "        echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.zshrc   # or ~/.bashrc"
           echo ;;
    esac
    if [[ -n "$CLIENT_ADDR" ]]; then
        echo "    Load the daemon address and token in each shell:"
        echo
        echo "        echo 'source $envfile' >> ~/.zshrc   # or ~/.bashrc"
        echo
        echo "    Or use them right now:"
        echo
        echo "        source $envfile && crucible sandbox ls"
    else
        echo "    Point the client at your Linux daemon:"
        echo
        echo "        export CRUCIBLE_ADDR=https://YOUR-LINUX-HOST:7878"
        echo "        export CRUCIBLE_TOKEN=<key from: crucible daemon token add>"
        echo "        crucible sandbox ls"
    fi
    echo
    echo "    Wire it into Claude Code / Cursor as an MCP server:"
    echo
    echo "        command: crucible"
    echo "        args:    [\"mcp\", \"serve\"]"
    echo "        env:     CRUCIBLE_ADDR, CRUCIBLE_TOKEN"
}

if [[ "$CLIENT" -eq 1 ]]; then
    install_client
    exit 0
fi

# ============================================================================
# Daemon install (Linux) — everything below requires root + systemd.
# ============================================================================

# maybe_download fetches the release tarball when the packaging files aren't
# next to this script — i.e. when piped from `curl | sh`. From a repo checkout
# or an extracted tarball, it's a no-op and we install from local files.
maybe_download() {
    [[ -f "$ROOT/packaging/crucible.service" ]] && return 0
    [[ "$BINARY_SET" -eq 1 ]] && return 0

    command -v curl >/dev/null || die "curl is required for download mode"
    command -v tar  >/dev/null || die "tar is required for download mode"

    local tag
    tag=$(resolve_tag)
    local pkg="crucible_${tag}_linux_amd64"
    local url="https://github.com/$REPO/releases/download/$tag/${pkg}.tar.gz"
    local tmp
    tmp="$(mktemp -d)"

    echo "==> downloading crucible $tag"
    fetch "$url" "$tmp/${pkg}.tar.gz"
    if curl -fsSL "${url}.sha256" -o "$tmp/${pkg}.tar.gz.sha256" 2>/dev/null; then
        ( cd "$tmp" && sha256sum -c "${pkg}.tar.gz.sha256" >/dev/null ) \
            || die "checksum verification failed for ${pkg}.tar.gz"
        echo "    checksum verified"
    fi
    tar -C "$tmp" -xzf "$tmp/${pkg}.tar.gz"
    ROOT="$tmp/$pkg"
    BINARY="$ROOT/crucible"
}

# provision_deps fetches firecracker+jailer, a default rootfs, and a guest
# kernel into the paths the config expects. Opt-in (--with-deps),
# checksum-verified, and skips any piece already present. On x86_64 the kernel
# comes from our own pinned release asset (firecracker-CI bucket as fallback);
# other arches need KERNEL_URL.
provision_deps() {
    local fc_arch
    case "$(uname -m)" in
        x86_64|amd64)  fc_arch=x86_64 ;;
        aarch64|arm64) fc_arch=aarch64 ;;
        *) die "--with-deps: unsupported arch $(uname -m)" ;;
    esac

    # 1) firecracker + jailer (from the official releases; verified sidecar).
    if [[ -x "$BINDIR/firecracker" && -x "$BINDIR/jailer" ]]; then
        kv deps "firecracker + jailer already present ($BINDIR) — skipped"
    else
        local tgz="firecracker-${FC_VERSION}-${fc_arch}.tgz"
        local base="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
        local tmp; tmp="$(mktemp -d)"
        kv deps "fetching firecracker ${FC_VERSION} (${fc_arch})"
        fetch "${base}/${tgz}" "$tmp/$tgz"
        verify_sha256 "$tmp/$tgz" "${base}/${tgz}.sha256.txt"
        tar -C "$tmp" -xzf "$tmp/$tgz"
        local d="$tmp/release-${FC_VERSION}-${fc_arch}"
        install -Dm755 "$d/firecracker-${FC_VERSION}-${fc_arch}" "$BINDIR/firecracker"
        install -Dm755 "$d/jailer-${FC_VERSION}-${fc_arch}"       "$BINDIR/jailer"
        rm -rf "$tmp"
        kv deps "installed firecracker + jailer -> $BINDIR"
    fi

    # 2) default rootfs (from crucible's own releases; amd64 profiles only).
    if [[ -f "$STATEDIR/rootfs.ext4" ]]; then
        kv deps "rootfs already present ($STATEDIR/rootfs.ext4) — skipped"
    elif [[ "$fc_arch" != x86_64 ]]; then
        warn "--with-deps: no prebuilt rootfs for $fc_arch — build one with 'make profile' or supply --rootfs"
    else
        local tag; tag=$(resolve_tag)
        local url="https://github.com/$REPO/releases/download/$tag/${ROOTFS_PROFILE}.ext4"
        local tmp; tmp="$(mktemp -d)"
        kv deps "fetching rootfs profile '${ROOTFS_PROFILE}' ($tag)"
        fetch "$url" "$tmp/rootfs.ext4"
        verify_sha256 "$tmp/rootfs.ext4" "${url}.sha256"
        install -Dm644 "$tmp/rootfs.ext4" "$STATEDIR/rootfs.ext4"
        rm -rf "$tmp"
        kv deps "installed rootfs -> $STATEDIR/rootfs.ext4"
    fi

    # 3) guest kernel. Prefer our own pinned release asset (supply-chain
    #    independence), fall back to the firecracker-CI bucket if the release
    #    predates it; override with KERNEL_URL / KERNEL_SHA256. x86_64 only by
    #    default (a matching rootfs only exists for x86_64).
    local kurl="${KERNEL_URL:-}" ksha="${KERNEL_SHA256:-}" kfallback=""
    if [[ -z "$kurl" && "$fc_arch" == x86_64 ]]; then
        kurl="https://github.com/$REPO/releases/download/$(resolve_tag)/vmlinux-x86_64"
        kfallback="$DEFAULT_KERNEL_FALLBACK_URL"
        ksha="$DEFAULT_KERNEL_SHA256"
    fi
    if [[ -f "$STATEDIR/vmlinux" ]]; then
        kv deps "kernel already present ($STATEDIR/vmlinux) — skipped"
    elif [[ -n "$kurl" ]]; then
        local tmp; tmp="$(mktemp -d)"
        kv deps "fetching guest kernel"
        if curl -fSL "$kurl" -o "$tmp/vmlinux" 2>/dev/null; then
            :
        elif [[ -n "$kfallback" ]] && curl -fSL "$kfallback" -o "$tmp/vmlinux" 2>/dev/null; then
            kv deps "(release kernel asset absent — used firecracker-CI fallback)"
        else
            die "kernel download failed: $kurl"
        fi
        [[ -n "$ksha" ]] && verify_sha256 "$tmp/vmlinux" "$ksha"
        install -Dm644 "$tmp/vmlinux" "$STATEDIR/vmlinux"
        rm -rf "$tmp"
        kv deps "installed kernel -> $STATEDIR/vmlinux"
    else
        warn "--with-deps: no prebuilt kernel for $fc_arch. Set KERNEL_URL to an"
        warn "  uncompressed vmlinux (and optional KERNEL_SHA256), then re-run."
    fi
}

EXAMPLE_POLICY="$CONFDIR/policies/example.json"

# write_example_policy drops an inert, editable scoped-policy template so the
# guidance below can point at a real file to `cat` and copy. Writes nothing if
# it already exists; mints no key and grants nothing on its own.
write_example_policy() {
    install -d -m 0750 "$CONFDIR/policies"
    [[ -f "$EXAMPLE_POLICY" ]] && return 0
    # Every operation, but resource-capped. Copy + tighten for real tokens
    # (drop "delete", lower caps, add "net_allow_max", set expiry with --ttl).
    cat > "$EXAMPLE_POLICY" <<'JSON'
{
  "operations": ["create", "exec", "snapshot", "fork", "delete", "read"],
  "max_sandboxes": 25,
  "max_fork": 16,
  "max_timeout_s": 3600
}
JSON
    chmod 640 "$EXAMPLE_POLICY"
}

# print_remote_guidance explains how to use the daemon locally and how to open
# it to another machine — informational only. It mints no key and changes no
# state, so a plain install stays zero-surprise.
print_remote_guidance() {
    local listen port
    listen="$(grep -o -- '--listen [^ "]*' "$CONFDIR/crucible.env" 2>/dev/null | awk '{print $2}')" || true
    listen="${listen:-127.0.0.1:7878}"; port="${listen##*:}"
    cat <<EOF

==> Using crucible

    On this machine the CLI already works — no key, no config needed:

        crucible sandbox ls

    To drive it from another machine (your Mac, a CI runner, ...):

        1. review or edit the example scoped policy:
             cat $EXAMPLE_POLICY

        2. mint a key bound to it (this turns on auth for every client):
             sudo crucible daemon token add --name my-laptop --policy $EXAMPLE_POLICY

        3. make the daemon reachable: set --listen to a routable address plus
           --tls-cert / --tls-key in $CONFDIR/crucible.env, then restart it.

        4. on the client machine:
             curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh \\
               | sh -s -- --client --addr https://THIS-HOST:$port --token <key>

    Or let the installer do steps 1-2 and print the exact client command
    plus an MCP config block to paste:

        $RERUN --connect-token
EOF
}

# --- preflight ----------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || die "must run as root (try: sudo $0 $*) — or install the client with --client"
command -v systemctl >/dev/null || die "systemctl not found — this installer targets systemd hosts (client install: --client)"

# The daemon shells out to e2fsprogs: mkfs.ext4 / fsck.ext4 / debugfs to convert
# OCI images into rootfs artifacts, and resize2fs for `--disk`. Missing tools
# don't block a profile-only install, so warn rather than fail.
missing_tools=""
for t in mkfs.ext4 fsck.ext4 debugfs resize2fs; do
    command -v "$t" >/dev/null 2>&1 || missing_tools="$missing_tools $t"
done
if [[ -n "$missing_tools" ]]; then
    warn "missing e2fsprogs tools:$missing_tools"
    warn "  booting OCI images (\`crucible run <image>\`) and \`--disk\` will fail."
    warn "  install with:  apt-get install -y e2fsprogs   # or: dnf install e2fsprogs"
fi

maybe_download

[[ -x "$BINARY" ]] || die "crucible binary not found at $BINARY (run 'make build' first, or pass --binary PATH)"
[[ -f "$ROOT/packaging/crucible.service" ]] || die "packaging/crucible.service missing next to $BINARY"

echo "==> Installing crucible"
kv binary "$BINARY -> $BINDIR/crucible"
install -Dm755 "$BINARY" "$BINDIR/crucible"

# profiles/ = pre-baked <profile>.ext4; images/ = converted OCI image cache
# (--image-dir); logs/ = durable per-sandbox logs (--log-dir). The daemon
# creates the latter two on demand, but pre-creating keeps ownership/mode sane.
install -d -m 0755 "$STATEDIR" "$STATEDIR/profiles" "$STATEDIR/images" "$STATEDIR/logs"

kv unit "$UNITDIR/crucible.service"
install -Dm644 "$ROOT/packaging/crucible.service" "$UNITDIR/crucible.service"

# Enable lazy fork (userfaultfd for the jailed, uid-dropped Firecracker).
if [[ ! -f /etc/sysctl.d/99-crucible.conf ]]; then
    echo 'vm.unprivileged_userfaultfd=1' > /etc/sysctl.d/99-crucible.conf
    if sysctl -p /etc/sysctl.d/99-crucible.conf >/dev/null 2>&1; then
        kv sysctl "vm.unprivileged_userfaultfd=1 (lazy fork enabled)"
    else
        warn "could not apply vm.unprivileged_userfaultfd — fork may be unavailable on this kernel"
    fi
fi

# apply_config_flags adds any missing daemon flags to CRUCIBLE_FLAGS in place,
# so an upgraded config doesn't silently disable features.
apply_config_flags() {
    local cfg="$1" changed=0
    local egress; egress="$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
    add_flag() { # flag value
        flags_have "$1" "$cfg" && return 0
        sed -i "s#^\\(CRUCIBLE_FLAGS=\"[^\"]*\\)\"#\\1 $1 $2\"#" "$cfg"
        kv config "added $1 $2"
        changed=1
    }
    add_flag "--image-dir" "$STATEDIR/images"
    add_flag "--log-dir"   "$STATEDIR/logs"
    add_flag "--app-db"    "$STATEDIR/apps.db"
    [[ -n "$egress" && "$NO_EGRESS_AUTO" -eq 0 ]] && add_flag "--network-egress-iface" "$egress"
    return $changed
}

# Config template — never clobber an operator's existing config.
install -d -m 0755 "$CONFDIR"
if [[ -e "$CONFDIR/crucible.env" ]]; then
    kv config "$CONFDIR/crucible.env (exists, left unchanged)"
    if [[ "$UPGRADE_CONFIG" -eq 1 ]]; then
        if apply_config_flags "$CONFDIR/crucible.env"; then
            kv config "already complete — nothing to add"
        else
            kv config "updated — restart to apply: sudo systemctl restart crucible"
        fi
    elif ! flags_have "--image-dir" "$CONFDIR/crucible.env"; then
        # An upgrade from a pre-image release leaves a config without --image-dir,
        # which silently disables `crucible run <image>` / `build` / /images.
        echo
        warn "your existing config predates OCI image support and has no --image-dir."
        warn "Until you add it, \`crucible run <image>\`, \`sandbox create --image\`"
        warn "and \`crucible build\` will fail. Apply the missing flags automatically:"
        echo >&2
        echo "  $RERUN --upgrade-config" >&2
        echo "  sudo systemctl restart crucible" >&2
        echo >&2
    fi
else
    install -Dm644 "$ROOT/packaging/crucible.env.example" "$CONFDIR/crucible.env"
    # A fresh config is complete out of the box: fold in the host's own
    # default-route NIC so *sandbox* egress (behind the allowlist) and
    # `run -p HOST:GUEST` port publish work without a manual edit. This is the
    # host's existing NIC, not a surprising choice; suppress with --no-egress-auto.
    egress="$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
    if [[ -n "$egress" && "$NO_EGRESS_AUTO" -eq 0 ]] && ! flags_have "--network-egress-iface" "$CONFDIR/crucible.env"; then
        sed -i "s#^\\(CRUCIBLE_FLAGS=\"[^\"]*\\)\"#\\1 --network-egress-iface $egress\"#" "$CONFDIR/crucible.env"
        kv config "$CONFDIR/crucible.env (from template; sandbox egress NIC = $egress)"
    else
        kv config "$CONFDIR/crucible.env (from template)"
        [[ "$NO_EGRESS_AUTO" -eq 1 ]] && kv config "egress NIC not set (--no-egress-auto) — add --network-egress-iface for sandbox net + run -p"
    fi
fi

# Drop the inert example scoped policy so the guidance can point at a real file.
write_example_policy

systemctl daemon-reload

if [[ "$WITH_DEPS" -eq 1 ]]; then
    echo "==> provisioning dependencies (--with-deps)"
    provision_deps
fi

if [[ "$ENABLE" -eq 1 ]]; then
    systemctl enable --now crucible
    echo "==> crucible enabled and started"
    kv status "systemctl status crucible"
    kv logs "journalctl -u crucible -f"
else
    cat <<EOF
==> installed (not started)

crucible needs a Firecracker binary, a guest kernel, and a rootfs — it does not
bundle those. Re-run with --with-deps to fetch firecracker+jailer, a kernel, and
a rootfs automatically, or put them at the paths $CONFDIR/crucible.env expects:

  firecracker : /usr/local/bin/firecracker   (and /usr/local/bin/jailer)
  kernel      : $STATEDIR/vmlinux
  rootfs      : $STATEDIR/rootfs.ext4
  profiles    : $STATEDIR/profiles/<name>.ext4
  images      : $STATEDIR/images            (created — converted OCI image cache)
  logs        : $STATEDIR/logs              (created — durable per-sandbox logs)

  Firecracker + jailer          : https://github.com/firecracker-microvm/firecracker/releases
  kernel + profile images + docs: https://github.com/$REPO

To boot OCI images (\`crucible run nginx:alpine\`) you also need e2fsprogs.

Then start it:
  sudo systemctl enable --now crucible
  journalctl -u crucible -f
EOF
fi

# A plain install mints nothing: just show how to use it locally and how to open
# it to another machine. --connect-token is the opt-in that actually does it.
if [[ "$CONNECT_TOKEN" -ne 1 ]]; then
    print_remote_guidance
    exit 0
fi

# --- --connect-token: mint a scoped token + print a ready-to-paste setup ---
tokenfile="$STATEDIR/tokens.json"
policyfile="$EXAMPLE_POLICY"        # the same example scoped policy install wrote
write_example_policy

echo
echo "==> Minting a scoped key"
echo
echo "    Policy ($policyfile):"
echo
sed 's/^/        /' "$policyfile"

raw=$("$BINDIR/crucible" daemon token add \
    --token-file "$tokenfile" --name "$TOKEN_NAME" --policy "$policyfile" 2>/dev/null \
    | awk '/key created/{f=1; next} f && NF {print $1; exit}') || true

# Derive the address to advertise from the config's --listen.
# (guard: grep finding nothing must not trip set -e under pipefail)
listen="$(grep -o -- '--listen [^ "]*' "$CONFDIR/crucible.env" 2>/dev/null | awk '{print $2}')" || true
listen="${listen:-127.0.0.1:7878}"
advertise="$listen"
case "$listen" in 127.0.0.1:*|localhost:*) advertise="https://YOUR-LINUX-HOST:${listen##*:}" ;; esac

echo
echo "==> Connect a remote client (e.g. your Mac)"
echo
if [[ -n "$raw" ]]; then
    echo "    Scoped API key '$TOKEN_NAME' — copy it now, it is not shown again:"
    echo
    echo "        $raw"
else
    echo "    Could not mint a token (is $BINDIR/crucible the new build?). Mint one with:"
    echo
    echo "        sudo crucible daemon token add --name $TOKEN_NAME --policy $policyfile"
fi
echo
echo "    1. On your Mac / Windows box, install the client and point it here:"
echo
echo "        curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh \\"
echo "          | sh -s -- --client --addr $advertise --token ${raw:-<key>}"
echo
echo "    2. Or wire it into Claude Code / Cursor as an MCP server:"
echo
sed 's/^/        /' <<JSON
{
  "mcpServers": {
    "crucible": {
      "command": "crucible",
      "args": ["mcp", "serve"],
      "env": {
        "CRUCIBLE_ADDR": "$advertise",
        "CRUCIBLE_TOKEN": "${raw:-<key>}"
      }
    }
  }
}
JSON
echo
echo "    Note: minting a key turns on auth for ALL clients, including local ones."
case "$listen" in
    127.0.0.1:*|localhost:*)
        echo "    Note: the daemon still listens on $listen (loopback). For remote access,"
        echo "          set --listen to a reachable address AND --tls-cert / --tls-key in"
        echo "          $CONFDIR/crucible.env, then: sudo systemctl restart crucible" ;;
esac
