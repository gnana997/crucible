#!/usr/bin/env bash
# install.sh — install the crucible daemon as a systemd service.
#
# Two ways to run:
#   # Users — download the latest release binary and install the service:
#   curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo sh
#
#   # Contributors — from a repo checkout or an extracted release tarball:
#   sudo ./install.sh [--enable]
#
# Flags:
#   --enable        enable + start the service now (default: install only)
#   --version TAG   download this release tag (default: latest; download mode)
#   --binary PATH   install this local binary instead of downloading
#
# Runs as root but never calls sudo itself. Env overrides: PREFIX (/usr/local).

set -euo pipefail

REPO="gnana997/crucible"

PREFIX="${PREFIX:-/usr/local}"
BINDIR="$PREFIX/bin"
UNITDIR="/etc/systemd/system"
CONFDIR="/etc/crucible"
STATEDIR="/var/lib/crucible"

ROOT="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo /nonexistent)"
BINARY="$ROOT/crucible"
BINARY_SET=0
VERSION=""
ENABLE=0

die() { echo "install: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --enable) ENABLE=1; shift ;;
        --version) VERSION="${2:?--version needs a tag}"; shift 2 ;;
        --binary) BINARY="${2:?--binary needs a path}"; BINARY_SET=1; shift 2 ;;
        -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
        *) die "unknown argument: $1 (see --help)" ;;
    esac
done

# maybe_download fetches the release tarball when the packaging files aren't
# next to this script — i.e. when piped from `curl | sh`. From a repo checkout
# or an extracted tarball, it's a no-op and we install from local files.
maybe_download() {
    [[ -f "$ROOT/packaging/crucible.service" ]] && return 0
    [[ "$BINARY_SET" -eq 1 ]] && return 0

    command -v curl >/dev/null || die "curl is required for download mode"
    command -v tar  >/dev/null || die "tar is required for download mode"

    local tag="$VERSION"
    if [[ -z "$tag" ]]; then
        tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
              | grep -m1 '"tag_name"' | cut -d'"' -f4) || true
        [[ -n "$tag" ]] || die "could not resolve the latest release for $REPO (pass --version)"
    fi

    local pkg="crucible_${tag}_linux_amd64"
    local url="https://github.com/$REPO/releases/download/$tag/${pkg}.tar.gz"
    local tmp
    tmp="$(mktemp -d)"

    echo "==> downloading crucible $tag"
    curl -fSL "$url" -o "$tmp/${pkg}.tar.gz" || die "download failed: $url"
    if curl -fsSL "${url}.sha256" -o "$tmp/${pkg}.tar.gz.sha256" 2>/dev/null; then
        ( cd "$tmp" && sha256sum -c "${pkg}.tar.gz.sha256" >/dev/null ) \
            || die "checksum verification failed for ${pkg}.tar.gz"
        echo "    checksum verified"
    fi
    tar -C "$tmp" -xzf "$tmp/${pkg}.tar.gz"
    ROOT="$tmp/$pkg"
    BINARY="$ROOT/crucible"
}

# --- preflight ----------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || die "must run as root (try: sudo $0 $*)"
command -v systemctl >/dev/null || die "systemctl not found — this installer targets systemd hosts"

maybe_download

[[ -x "$BINARY" ]] || die "crucible binary not found at $BINARY (run 'make build' first, or pass --binary PATH)"
[[ -f "$ROOT/packaging/crucible.service" ]] || die "packaging/crucible.service missing next to $BINARY"

echo "==> installing crucible"
echo "    binary : $BINARY -> $BINDIR/crucible"
install -Dm755 "$BINARY" "$BINDIR/crucible"

install -d -m 0755 "$STATEDIR" "$STATEDIR/profiles"

echo "    unit   : $UNITDIR/crucible.service"
install -Dm644 "$ROOT/packaging/crucible.service" "$UNITDIR/crucible.service"

# Config template — never clobber an operator's existing config.
install -d -m 0755 "$CONFDIR"
if [[ -e "$CONFDIR/crucible.env" ]]; then
    echo "    config : $CONFDIR/crucible.env (exists, left unchanged)"
else
    install -Dm644 "$ROOT/packaging/crucible.env.example" "$CONFDIR/crucible.env"
    echo "    config : $CONFDIR/crucible.env (from template — edit before starting)"
fi

systemctl daemon-reload

if [[ "$ENABLE" -eq 1 ]]; then
    systemctl enable --now crucible
    echo "==> crucible enabled and started"
    echo "    status : systemctl status crucible"
    echo "    logs   : journalctl -u crucible -f"
else
    cat <<EOF
==> installed (not started)

Next steps:
  1. Get a Firecracker binary + guest kernel (see the README) and a rootfs
     (a release profile image, or 'make rootfs' / 'make profile').
  2. Edit $CONFDIR/crucible.env with those paths.
  3. sudo systemctl enable --now crucible
  4. journalctl -u crucible -f          # watch it boot
EOF
fi
