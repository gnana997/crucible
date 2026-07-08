#!/usr/bin/env bash
# scripts/build-profile.sh — build a native crucible rootfs profile.
#
# Reads profiles/profiles.env for <profile> -> <base OCI image>, builds a
# container (profiles/Dockerfile) that injects systemd + the crucible
# agent + crucible's network config into that native base, exports the
# container filesystem, and packs it into an ext4 image Firecracker boots.
#
# Usage:
#   make agent                              # build bin/crucible-agent first
#   ./scripts/build-profile.sh python-3.12 [out-dir]
#
# Output : <out-dir>/<profile>.ext4         (default out-dir: assets/profiles)
# Serve  : crucible daemon --rootfs-dir <out-dir> ...
#
# Requires: docker (buildkit), fakeroot, mkfs.ext4 + debugfs (e2fsprogs).
# No host-side sudo: the exported root:root ownership is preserved into
# the ext4 via a single fakeroot session, same trick as build-rootfs.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MANIFEST="$ROOT/profiles/profiles.env"
DOCKERFILE="$ROOT/profiles/Dockerfile"
AGENT="$ROOT/bin/crucible-agent"

die() { echo "build-profile: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || die "usage: $0 <profile> [out-dir]"
PROFILE="$1"
OUTDIR="${2:-$ROOT/assets/profiles}"

# --- preflight ----------------------------------------------------------
command -v docker    >/dev/null || die "docker not installed"
command -v fakeroot  >/dev/null || die "fakeroot not installed (apt install fakeroot)"
command -v mkfs.ext4 >/dev/null || die "mkfs.ext4 not installed (apt install e2fsprogs)"
command -v debugfs   >/dev/null || die "debugfs not installed (apt install e2fsprogs)"

[[ -f "$MANIFEST"   ]] || die "manifest not found: $MANIFEST"
[[ -f "$DOCKERFILE" ]] || die "Dockerfile not found: $DOCKERFILE"
[[ -x "$AGENT"      ]] || die "agent binary not found: $AGENT (run: make agent)"

# Resolve the base image from the manifest (skip comments / blank lines).
BASE="$(awk -v p="$PROFILE" '!/^#/ && $1==p {print $2; exit}' "$MANIFEST")"
[[ -n "$BASE" ]] || die "unknown profile '$PROFILE' (not listed in $MANIFEST)"

echo "==> build-profile: $PROFILE   (base: $BASE)"

ctx="$(mktemp -d)"
cid=""
cleanup() {
    [[ -n "$cid" ]] && docker rm -f "$cid" >/dev/null 2>&1 || true
    rm -rf "$ctx"
}
trap cleanup EXIT

# Tiny build context: just the agent + the Dockerfile.
cp "$AGENT" "$ctx/crucible-agent"
cp "$DOCKERFILE" "$ctx/Dockerfile"

img="crucible-profile:$PROFILE"
echo "--> docker build ($img)"
# --network=host: RUN steps share the host netns, so apt resolves DNS
# through the host's resolver (e.g. systemd-resolved's 127.0.0.53 stub).
# Docker's default bridge falls back to 8.8.8.8 when the host resolv.conf
# only lists a localhost stub, which breaks on networks that block
# direct outbound DNS (VPNs, filtering routers). This is a local build
# script producing a rootfs — build-time network isolation buys nothing.
DOCKER_BUILDKIT=1 docker build --network=host --build-arg "BASE=$BASE" -t "$img" "$ctx"

echo "--> exporting container filesystem"
cid="$(docker create "$img")"
docker export "$cid" > "$ctx/rootfs.tar"

mkdir -p "$OUTDIR"
OUT="$OUTDIR/$PROFILE.ext4"

echo "--> packing ext4: $OUT"
fakeroot bash <<FR
set -e
rm -rf "$ctx/root"
mkdir "$ctx/root"
tar -C "$ctx/root" -xf "$ctx/rootfs.tar"
# /etc/resolv.conf can't be baked from the Dockerfile — Docker bind-mounts it
# during build, so writes there never land in the image layer. Write it into
# the exported tree instead, pointing at the daemon's DNS anycast address.
cat > "$ctx/root/etc/resolv.conf" <<'RESOLV'
# Managed by crucible's profile build.
nameserver 10.20.255.254
options edns0
RESOLV
# Size from the extracted tree + 40% headroom, floor 1 GiB.
bytes=\$(du -sb "$ctx/root" | cut -f1)
mib=\$(( bytes / 1024 / 1024 * 14 / 10 + 128 ))
if [ "\$mib" -lt 1024 ]; then mib=1024; fi
truncate -s "\${mib}M" "$OUT.partial"
mkfs.ext4 -q -d "$ctx/root" -F "$OUT.partial"
FR
mv "$OUT.partial" "$OUT"

echo "--> verifying"
debugfs -R "stat /usr/local/bin/crucible-agent" "$OUT" >/dev/null 2>&1 \
    || die "agent missing in $OUT"
debugfs -R "stat /etc/systemd/system/multi-user.target.wants/crucible-agent.service" "$OUT" >/dev/null 2>&1 \
    || die "agent service not enabled in $OUT"

ls -lh "$OUT"
echo "==> ok: $OUT   (profile: $PROFILE)"
