#!/usr/bin/env bash
# scripts/build-rootfs.sh
#
# Bake crucible-agent into a guest rootfs image.
#
# Inputs  : a base Ubuntu squashfs (e.g. from Firecracker's CI bucket)
#           and the compiled crucible-agent binary.
# Output  : an ext4 rootfs with:
#             /usr/local/bin/crucible-agent                                  (installed)
#             /etc/systemd/system/crucible-agent.service                     (unit)
#             /etc/systemd/system/multi-user.target.wants/crucible-agent.*   (enabled)
# Strategy: unsquashfs + file-tree edits + `mkfs.ext4 -d` — all inside a
#           single fakeroot session so the resulting ext4 owns files as
#           root:root inside the guest without needing host-side sudo.
#
# Example:
#   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
#     go build -o bin/crucible-agent ./cmd/crucible-agent
#   ./scripts/build-rootfs.sh assets/ubuntu-24.04.squashfs bin/crucible-agent \
#     assets/rootfs.ext4

set -euo pipefail

usage() {
    cat >&2 <<EOF
Usage: $0 <base-squashfs> <agent-binary> <output-ext4> [size]

  <base-squashfs>   Ubuntu rootfs in squashfs format (input)
  <agent-binary>    crucible-agent ELF binary, x86_64 Linux
  <output-ext4>     Target ext4 path — overwritten if it exists
  [size]            Image size passed to truncate (default: 1G)

Requires: fakeroot, unsquashfs (squashfs-tools), mkfs.ext4 + debugfs (e2fsprogs).
EOF
}

die() { echo "build-rootfs: $*" >&2; exit 1; }

if [[ $# -lt 3 || $# -gt 4 ]]; then
    usage
    exit 2
fi

BASE="$1"
AGENT="$2"
OUT="$3"
SIZE="${4:-1G}"

# --- preflight ----------------------------------------------------------
command -v fakeroot   >/dev/null || die "fakeroot not installed (apt install fakeroot)"
command -v unsquashfs >/dev/null || die "unsquashfs not installed (apt install squashfs-tools)"
command -v mkfs.ext4  >/dev/null || die "mkfs.ext4 not installed (apt install e2fsprogs)"
command -v debugfs    >/dev/null || die "debugfs not installed (apt install e2fsprogs)"

[[ -r "$BASE"  ]] || die "base rootfs not readable: $BASE"
[[ -f "$AGENT" ]] || die "agent binary not found: $AGENT"
[[ -x "$AGENT" ]] || die "agent binary is not executable: $AGENT"

# Warn (don't fail) if the agent doesn't look like a linux/amd64 ELF.
if command -v file >/dev/null; then
    if ! file "$AGENT" | grep -q 'ELF 64-bit LSB.*x86-64'; then
        echo "build-rootfs: warning: $AGENT is not linux/amd64:" >&2
        file "$AGENT" >&2
    fi
fi

# Make OUT absolute so the fakeroot-spawned shell can reach it after it
# cd's into temp directories if we ever add that later.
OUT="$(cd "$(dirname "$OUT")" && pwd)/$(basename "$OUT")"
BASE="$(cd "$(dirname "$BASE")" && pwd)/$(basename "$BASE")"
AGENT="$(cd "$(dirname "$AGENT")" && pwd)/$(basename "$AGENT")"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "==> build-rootfs"
echo "    base   : $BASE"
echo "    agent  : $AGENT"
echo "    output : $OUT"
echo "    size   : $SIZE"

# --- do it all inside one fakeroot session ------------------------------
# A single fakeroot session preserves the uid/gid fakeroot assigns (0/0
# for everything unsquashfs creates) through the subsequent mkfs.ext4
# read of the tree. No host-side sudo required.
fakeroot bash <<FR
set -e

echo "--> extracting base rootfs"
unsquashfs -q -d "$tmp/root" "$BASE"

echo "--> installing /usr/local/bin/crucible-agent"
install -Dm755 "$AGENT" "$tmp/root/usr/local/bin/crucible-agent"

echo "--> writing systemd unit"
mkdir -p "$tmp/root/etc/systemd/system"
cat > "$tmp/root/etc/systemd/system/crucible-agent.service" <<'UNIT'
[Unit]
Description=crucible guest agent (vsock exec endpoint)
After=basic.target
Wants=basic.target
ConditionPathExists=/usr/local/bin/crucible-agent

[Service]
Type=simple
ExecStart=/usr/local/bin/crucible-agent
Restart=on-failure
RestartSec=1
# journal+console so wk2 boot-log debugging is visible on ttyS0; will
# drop the console suffix once the agent path is stable.
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
UNIT

echo "--> enabling service (multi-user.target.wants symlink)"
mkdir -p "$tmp/root/etc/systemd/system/multi-user.target.wants"
ln -sf ../crucible-agent.service \
    "$tmp/root/etc/systemd/system/multi-user.target.wants/crucible-agent.service"

echo "--> packing ext4 image ($SIZE)"
truncate -s "$SIZE" "$OUT.partial"
mkfs.ext4 -q -d "$tmp/root" -F "$OUT.partial"
FR

mv "$OUT.partial" "$OUT"

# --- verify -------------------------------------------------------------
echo "--> verifying"
debugfs -R "stat /usr/local/bin/crucible-agent" "$OUT" >/dev/null 2>&1 \
    || die "agent not present in output rootfs"
debugfs -R "stat /etc/systemd/system/multi-user.target.wants/crucible-agent.service" "$OUT" >/dev/null 2>&1 \
    || die "systemd enable symlink missing in output rootfs"

ls -lh "$OUT"
echo "==> ok: $OUT"
