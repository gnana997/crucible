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
# Flags:
#   --client            install just the CLI (+ `mcp serve`); no daemon/systemd/root
#   --addr URL          (client) default daemon address → CRUCIBLE_ADDR (e.g. https://host:7878)
#   --token TOK         (client) API key → CRUCIBLE_TOKEN
#   --enable            (daemon) enable + start the service now
#   --with-deps         (daemon) also fetch firecracker+jailer, a rootfs, (kernel: see notes) — opt-in, checksum-verified
#   --upgrade-config    (daemon) apply missing --image-dir / --log-dir / --network-egress-iface to an existing config
#   --connect-token     (daemon) mint a scoped token and print a ready-to-paste MCP config + client one-liner
#   --token-name NAME   (daemon) name for --connect-token's key (default: remote-client)
#   --version TAG       release tag (default: latest; download mode)
#   --binary PATH       install this local binary instead of downloading
#
# The daemon path runs as root but never calls sudo itself. Env overrides:
#   PREFIX (/usr/local), CLIENT_BINDIR (client install dir), FC_VERSION,
#   ROOTFS_PROFILE (default: base), KERNEL_URL / KERNEL_SHA256 (kernel source).

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

ROOT="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo /nonexistent)"
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

die()  { echo "install: $*" >&2; exit 1; }
warn() { echo "install: warning: $*" >&2; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --client) CLIENT=1; shift ;;
        --addr) CLIENT_ADDR="${2:?--addr needs a URL}"; shift 2 ;;
        --token) CLIENT_TOKEN="${2:?--token needs a key}"; shift 2 ;;
        --enable) ENABLE=1; shift ;;
        --with-deps) WITH_DEPS=1; shift ;;
        --upgrade-config) UPGRADE_CONFIG=1; shift ;;
        --connect-token) CONNECT_TOKEN=1; shift ;;
        --token-name) TOKEN_NAME="${2:?--token-name needs a name}"; shift 2 ;;
        --version) VERSION="${2:?--version needs a tag}"; shift 2 ;;
        --binary) BINARY="${2:?--binary needs a path}"; BINARY_SET=1; shift 2 ;;
        -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
        *) die "unknown argument: $1 (see --help)" ;;
    esac
done

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
    local file="$1" url="$2" want got
    want=$(curl -fsSL "$url" 2>/dev/null | awk 'NF{print $1; exit}') \
        || die "could not fetch checksum: $url"
    [[ -n "$want" ]] || die "empty checksum from $url"
    got=$(sha256sum "$file" | awk '{print $1}')
    [[ "$want" == "$got" ]] || die "checksum mismatch for $(basename "$file") (want $want, got $got)"
}

# ============================================================================
# Client install (macOS / Windows / Linux) — no root, no systemd.
# ============================================================================
install_client() {
    detect_platform

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
    echo "==> installed client: $out"
    "$out" version 2>/dev/null || true

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
    echo "==> next steps"
    case ":$PATH:" in
        *":$bindir:"*) : ;;
        *) echo "    • add $bindir to your PATH:"
           echo "        echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.zshrc   # or ~/.bashrc" ;;
    esac
    if [[ -n "$CLIENT_ADDR" ]]; then
        echo "    • load the daemon address/token in each shell:"
        echo "        echo 'source $envfile' >> ~/.zshrc   # or ~/.bashrc"
        echo "    • or use them right now:"
        echo "        source $envfile && crucible sandbox ls"
    else
        echo "    • point the client at your Linux daemon:"
        echo "        export CRUCIBLE_ADDR=https://YOUR-LINUX-HOST:7878"
        echo "        export CRUCIBLE_TOKEN=<key from: crucible daemon token add>"
        echo "        crucible sandbox ls"
    fi
    echo "    • wire it into Claude Code / Cursor as an MCP server:"
    echo "        command: crucible   args: [\"mcp\",\"serve\"]   env: CRUCIBLE_ADDR + CRUCIBLE_TOKEN"
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

# provision_deps fetches firecracker+jailer and a default rootfs into the paths
# the config expects. Opt-in (--with-deps), checksum-verified, and skips any
# piece already present. The guest kernel has no published release asset yet,
# so it's fetched only when KERNEL_URL is set — otherwise we report it as the
# one remaining manual piece.
provision_deps() {
    local fc_arch
    case "$(uname -m)" in
        x86_64|amd64)  fc_arch=x86_64 ;;
        aarch64|arm64) fc_arch=aarch64 ;;
        *) die "--with-deps: unsupported arch $(uname -m)" ;;
    esac

    # 1) firecracker + jailer (from the official releases; verified sidecar).
    if [[ -x "$BINDIR/firecracker" && -x "$BINDIR/jailer" ]]; then
        echo "    deps: firecracker + jailer already present ($BINDIR) — skipped"
    else
        local tgz="firecracker-${FC_VERSION}-${fc_arch}.tgz"
        local base="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
        local tmp; tmp="$(mktemp -d)"
        echo "    deps: fetching firecracker ${FC_VERSION} (${fc_arch})"
        fetch "${base}/${tgz}" "$tmp/$tgz"
        verify_sha256 "$tmp/$tgz" "${base}/${tgz}.sha256.txt"
        tar -C "$tmp" -xzf "$tmp/$tgz"
        local d="$tmp/release-${FC_VERSION}-${fc_arch}"
        install -Dm755 "$d/firecracker-${FC_VERSION}-${fc_arch}" "$BINDIR/firecracker"
        install -Dm755 "$d/jailer-${FC_VERSION}-${fc_arch}"       "$BINDIR/jailer"
        rm -rf "$tmp"
        echo "    deps: installed firecracker + jailer -> $BINDIR"
    fi

    # 2) default rootfs (from crucible's own releases; amd64 profiles only).
    if [[ -f "$STATEDIR/rootfs.ext4" ]]; then
        echo "    deps: rootfs already present ($STATEDIR/rootfs.ext4) — skipped"
    elif [[ "$fc_arch" != x86_64 ]]; then
        warn "--with-deps: no prebuilt rootfs for $fc_arch — build one with 'make profile' or supply --rootfs"
    else
        local tag; tag=$(resolve_tag)
        local url="https://github.com/$REPO/releases/download/$tag/${ROOTFS_PROFILE}.ext4"
        local tmp; tmp="$(mktemp -d)"
        echo "    deps: fetching rootfs profile '${ROOTFS_PROFILE}' ($tag)"
        fetch "$url" "$tmp/rootfs.ext4"
        verify_sha256 "$tmp/rootfs.ext4" "${url}.sha256"
        install -Dm644 "$tmp/rootfs.ext4" "$STATEDIR/rootfs.ext4"
        rm -rf "$tmp"
        echo "    deps: installed rootfs -> $STATEDIR/rootfs.ext4"
    fi

    # 3) guest kernel — only if a source is configured (no release asset yet).
    if [[ -f "$STATEDIR/vmlinux" ]]; then
        echo "    deps: kernel already present ($STATEDIR/vmlinux) — skipped"
    elif [[ -n "${KERNEL_URL:-}" ]]; then
        local tmp; tmp="$(mktemp -d)"
        echo "    deps: fetching kernel from KERNEL_URL"
        fetch "$KERNEL_URL" "$tmp/vmlinux"
        [[ -n "${KERNEL_SHA256:-}" ]] && verify_sha256 "$tmp/vmlinux" "$KERNEL_SHA256"
        install -Dm644 "$tmp/vmlinux" "$STATEDIR/vmlinux"
        rm -rf "$tmp"
        echo "    deps: installed kernel -> $STATEDIR/vmlinux"
    else
        warn "--with-deps: no guest kernel provisioned (no published asset yet)."
        warn "  Supply one and re-run, or drop a vmlinux at $STATEDIR/vmlinux:"
        warn "    KERNEL_URL=<uncompressed vmlinux url> [KERNEL_SHA256=<url>] sudo ./install.sh --with-deps"
        warn "  Firecracker CI kernels: https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md"
    fi
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

echo "==> installing crucible"
echo "    binary : $BINARY -> $BINDIR/crucible"
install -Dm755 "$BINARY" "$BINDIR/crucible"

# profiles/ = pre-baked <profile>.ext4; images/ = converted OCI image cache
# (--image-dir); logs/ = durable per-sandbox logs (--log-dir). The daemon
# creates the latter two on demand, but pre-creating keeps ownership/mode sane.
install -d -m 0755 "$STATEDIR" "$STATEDIR/profiles" "$STATEDIR/images" "$STATEDIR/logs"

echo "    unit   : $UNITDIR/crucible.service"
install -Dm644 "$ROOT/packaging/crucible.service" "$UNITDIR/crucible.service"

# Enable lazy fork (userfaultfd for the jailed, uid-dropped Firecracker).
if [[ ! -f /etc/sysctl.d/99-crucible.conf ]]; then
    echo 'vm.unprivileged_userfaultfd=1' > /etc/sysctl.d/99-crucible.conf
    if sysctl -p /etc/sysctl.d/99-crucible.conf >/dev/null 2>&1; then
        echo "    sysctl : vm.unprivileged_userfaultfd=1 (lazy fork enabled)"
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
        grep -q -- "$1" "$cfg" && return 0
        sed -i "s#^\\(CRUCIBLE_FLAGS=\"[^\"]*\\)\"#\\1 $1 $2\"#" "$cfg"
        echo "    config : added $1 $2"
        changed=1
    }
    add_flag "--image-dir" "$STATEDIR/images"
    add_flag "--log-dir"   "$STATEDIR/logs"
    [[ -n "$egress" ]] && add_flag "--network-egress-iface" "$egress"
    return $changed
}

# Config template — never clobber an operator's existing config.
install -d -m 0755 "$CONFDIR"
if [[ -e "$CONFDIR/crucible.env" ]]; then
    echo "    config : $CONFDIR/crucible.env (exists, left unchanged)"
    if [[ "$UPGRADE_CONFIG" -eq 1 ]]; then
        if apply_config_flags "$CONFDIR/crucible.env"; then
            echo "    config : already complete — nothing to add"
        else
            echo "    config : updated — restart to apply: sudo systemctl restart crucible"
        fi
    elif ! grep -q -- "--image-dir" "$CONFDIR/crucible.env"; then
        # An upgrade from a pre-image release leaves a config without --image-dir,
        # which silently disables `crucible run <image>` / `build` / /images.
        echo
        warn "your existing config predates OCI image support and has no --image-dir."
        warn "Until you add it, \`crucible run <image>\`, \`sandbox create --image\`"
        warn "and \`crucible build\` will fail. Apply the missing flags automatically:"
        echo >&2
        echo "  sudo $0 --upgrade-config" >&2
        echo "  sudo systemctl restart crucible" >&2
        echo >&2
    fi
else
    install -Dm644 "$ROOT/packaging/crucible.env.example" "$CONFDIR/crucible.env"
    # A fresh config is complete out of the box: fold in the host's egress NIC
    # so sandbox networking + `run -p` work without a manual edit.
    egress="$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
    if [[ -n "$egress" ]] && ! grep -q -- "--network-egress-iface" "$CONFDIR/crucible.env"; then
        sed -i "s#^\\(CRUCIBLE_FLAGS=\"[^\"]*\\)\"#\\1 --network-egress-iface $egress\"#" "$CONFDIR/crucible.env"
        echo "    config : $CONFDIR/crucible.env (from template; egress NIC $egress wired in)"
    else
        echo "    config : $CONFDIR/crucible.env (from template — edit before starting)"
    fi
fi

systemctl daemon-reload

if [[ "$WITH_DEPS" -eq 1 ]]; then
    echo "==> provisioning dependencies (--with-deps)"
    provision_deps
fi

if [[ "$ENABLE" -eq 1 ]]; then
    systemctl enable --now crucible
    echo "==> crucible enabled and started"
    echo "    status : systemctl status crucible"
    echo "    logs   : journalctl -u crucible -f"
else
    cat <<EOF
==> installed (not started)

crucible needs a Firecracker binary, a guest kernel, and a rootfs — it does not
bundle those. Re-run with --with-deps to fetch firecracker+jailer+rootfs, or
put them at the paths $CONFDIR/crucible.env already expects:

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

# --- mint a scoped token + print a ready-to-paste remote-client setup ---
if [[ "$CONNECT_TOKEN" -eq 1 ]]; then
    tokenfile="$STATEDIR/tokens.json"
    policydir="$CONFDIR/policies"
    policyfile="$policydir/${TOKEN_NAME}.json"
    install -d -m 0750 "$policydir"
    if [[ ! -f "$policyfile" ]]; then
        # A bounded remote-developer scope: every operation, but capped. Edit to
        # tighten (drop "delete", lower the caps, add net_allow_max, ...).
        cat > "$policyfile" <<'JSON'
{
  "operations": ["create", "exec", "snapshot", "fork", "delete", "read"],
  "max_sandboxes": 25,
  "max_fork": 16,
  "max_timeout_s": 3600
}
JSON
        chmod 640 "$policyfile"
    fi

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
    echo "==============================================================="
    echo " connect a remote client (e.g. your Mac)"
    echo "==============================================================="
    if [[ -n "$raw" ]]; then
        echo "Scoped API key '$TOKEN_NAME' (policy: $policyfile). Copy it now — not shown again:"
        echo
        echo "    $raw"
    else
        warn "could not mint a token (is $BINDIR/crucible the new build?). Mint one with:"
        echo "    sudo crucible daemon token add --name $TOKEN_NAME --policy $policyfile" >&2
    fi
    echo
    echo "On your Mac / Windows box, install the client and point it here:"
    echo
    echo "    curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh \\"
    echo "      | sh -s -- --client --addr $advertise --token ${raw:-<key>}"
    echo
    echo "Then wire it into Claude Code / Cursor as an MCP server:"
    echo
    cat <<JSON
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
    echo "Note: minting a key turns ON auth for ALL clients (including local ones)."
    case "$listen" in
        127.0.0.1:*|localhost:*)
            warn "the daemon still listens on $listen (loopback). For real remote access,"
            warn "set --listen to a reachable address AND --tls-cert/--tls-key in $CONFDIR/crucible.env,"
            warn "then: sudo systemctl restart crucible" ;;
    esac
fi
