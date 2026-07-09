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

die()  { echo "install: $*" >&2; exit 1; }
warn() { echo "install: warning: $*" >&2; }

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
        # Fetch fully into a var first: piping curl into `grep -m1` makes grep
        # close the pipe early, which prints a spurious "curl: (23)" error.
        local api
        api=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest") \
            || die "could not query the latest release for $REPO (pass --version)"
        tag=$(printf '%s\n' "$api" | grep -m1 '"tag_name"' | cut -d'"' -f4)
        [[ -n "$tag" ]] || die "no published release found for $REPO (pass --version)"
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

# Config template — never clobber an operator's existing config.
install -d -m 0755 "$CONFDIR"
if [[ -e "$CONFDIR/crucible.env" ]]; then
    echo "    config : $CONFDIR/crucible.env (exists, left unchanged)"

    # An upgrade from a pre-image release leaves a config without --image-dir,
    # which silently disables `crucible run <image>` / `build` / the /images
    # API. We won't edit an operator's config, so say exactly what to add.
    if ! grep -q -- "--image-dir" "$CONFDIR/crucible.env"; then
        echo
        warn "your existing config predates OCI image support and has no --image-dir."
        warn "Until you add it, \`crucible run <image>\`, \`sandbox create --image\`"
        warn "and \`crucible build\` will fail. Add it with:"
        echo >&2
        echo "  sudo sed -i 's#^\\(CRUCIBLE_FLAGS=\"[^\"]*\\)\"#\\1 --image-dir $STATEDIR/images\"#' $CONFDIR/crucible.env" >&2
        echo "  sudo systemctl restart crucible" >&2
        echo >&2
    fi
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

crucible needs a Firecracker binary, a guest kernel, and a rootfs — it does not
bundle those. Put them at the paths $CONFDIR/crucible.env already expects:

  firecracker : /usr/local/bin/firecracker   (and /usr/local/bin/jailer)
  kernel      : $STATEDIR/vmlinux
  rootfs      : $STATEDIR/rootfs.ext4
  profiles    : $STATEDIR/profiles/<name>.ext4
  images      : $STATEDIR/images            (created — converted OCI image cache)
  logs        : $STATEDIR/logs              (created — durable per-sandbox logs)

  Firecracker + jailer          : https://github.com/firecracker-microvm/firecracker/releases
  kernel + profile images + docs: https://github.com/$REPO

To boot OCI images (\`crucible run nginx:alpine\`) you also need e2fsprogs.
For sandbox networking and \`run -p HOST:GUEST\` port publish, add your NIC to
$CONFDIR/crucible.env:

  --network-egress-iface \$(ip -4 route show default | awk '{print \$5; exit}')

Then start it:
  sudo systemctl enable --now crucible
  journalctl -u crucible -f
EOF
fi
