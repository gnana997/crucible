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

# Verify iproute2's `ip` is present — the crucible-agent's
# POST /network/refresh endpoint shells out to it to bounce eth0,
# which prompts systemd-networkd to run a fresh DHCP cycle. It's
# part of every Ubuntu/Debian base by default; this check catches
# an unusually minimal image early.
echo "--> verifying iproute2 (ip) is present"
if ! [[ -x "$tmp/root/bin/ip" || -x "$tmp/root/usr/bin/ip" || -x "$tmp/root/sbin/ip" || -x "$tmp/root/usr/sbin/ip" ]]; then
    die "iproute2 'ip' binary not found in base rootfs (install 'iproute2' package)"
fi

echo "--> installing /usr/local/bin/crucible-agent"
install -Dm755 "$AGENT" "$tmp/root/usr/local/bin/crucible-agent"

# Locate systemd-networkd. systemd-resolved is NOT required — we
# use a static /etc/resolv.conf pointing at the daemon's DNS
# anycast IP, which keeps the rootfs working on minimal base
# images that ship systemd-networkd without resolved.
NETWORKD_UNIT=""
for p in /usr/lib/systemd/system/systemd-networkd.service /lib/systemd/system/systemd-networkd.service; do
    if [[ -f "$tmp/root\$p" ]]; then NETWORKD_UNIT="\$p"; break; fi
done
[[ -n "\$NETWORKD_UNIT" ]] || { echo "systemd-networkd.service not found in rootfs" >&2; exit 1; }

NETWORKD_SOCK=""
for p in /usr/lib/systemd/system/systemd-networkd.socket /lib/systemd/system/systemd-networkd.socket; do
    if [[ -f "$tmp/root\$p" ]]; then NETWORKD_SOCK="\$p"; break; fi
done
[[ -n "\$NETWORKD_SOCK" ]] || { echo "systemd-networkd.socket not found in rootfs" >&2; exit 1; }

echo "    systemd-networkd.service at \$NETWORKD_UNIT"

# Drop a systemd-networkd .network file that tells it to DHCP on
# eth0. Bypasses netplan entirely — netplan would require
# `netplan generate` at boot (cloud-init or a custom service).
# Writing the .network file directly saves a translation layer.
echo "--> installing systemd-networkd eth0 DHCP config"
mkdir -p "$tmp/root/etc/systemd/network"
cat > "$tmp/root/etc/systemd/network/20-crucible-eth0.network" <<'NETWORK'
# Managed by crucible's rootfs build.
# systemd-networkd runs DHCP on eth0 at boot and re-runs it when
# the link bounces. POST /network/refresh on the agent does
# `ip link set eth0 down/up` so forks with stale snapshotted IP
# state re-DHCP to their per-netns-assigned address.
[Match]
Name=eth0

[Network]
DHCP=ipv4
LinkLocalAddressing=no
IPv6AcceptRA=no

# UseDNS=no tells networkd NOT to override our static
# /etc/resolv.conf with the DHCP-offered DNS servers. They happen
# to be the same IP either way (our responder hands out DNS =
# 10.20.255.254), but declaring it statically makes the resolver
# path independent of DHCP state — queries work even if DHCP
# hasn't finished yet.
[DHCP]
UseDNS=no
NETWORK

# Enable systemd-networkd via the manual-symlink pattern
# systemctl enable uses under the hood (we can't run systemctl
# from inside the build — no active systemd at rootfs-bake time).
echo "--> enabling systemd-networkd"
mkdir -p "$tmp/root/etc/systemd/system/multi-user.target.wants"
mkdir -p "$tmp/root/etc/systemd/system/sockets.target.wants"
ln -sf "..\$NETWORKD_UNIT" \\
    "$tmp/root/etc/systemd/system/multi-user.target.wants/systemd-networkd.service"
ln -sf "..\$NETWORKD_SOCK" \\
    "$tmp/root/etc/systemd/system/sockets.target.wants/systemd-networkd.socket"

# Write a static /etc/resolv.conf pointing at the daemon's DNS
# anycast IP. The daemon's default is 10.20.255.254; operators
# who change it via --dns-anycast (not exposed in v0.1) would
# need to rebuild the rootfs. This keeps the guest resolver
# working without systemd-resolved.
echo "--> installing CA certificate bundle"
# The minimal Firecracker CI squashfs ships without ca-certificates,
# so HTTPS clients in the guest (python ssl, curl, wget) fail with
# "unable to get local issuer certificate". We copy the host's
# system CA bundle in rather than apt-install ca-certificates inside
# a fakeroot — the host bundle is the same upstream Mozilla set and
# avoids dragging in apt/dpkg machinery inside this script.
#
# Python's ssl module (via OpenSSL) checks its OPENSSLDIR cert.pem
# (/usr/lib/ssl/cert.pem → /etc/ssl/cert.pem) AND the cert directory
# (/etc/ssl/certs/) for c_rehash-hashed files. A lone bundle at
# ca-certificates.crt isn't enough by itself, so we:
#   1. place the bundle at the canonical path
#   2. symlink /etc/ssl/cert.pem → the bundle (what OpenSSL reads by default)
#   3. export SSL_CERT_FILE globally so anything that ignores the
#      OpenSSL defaults still finds it
HOST_CA=""
for candidate in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    if [[ -r "\$candidate" ]]; then HOST_CA="\$candidate"; break; fi
done
if [[ -z "\$HOST_CA" ]]; then
    echo "build-rootfs: warning: no host CA bundle found; HTTPS will fail in the guest" >&2
else
    install -Dm644 "\$HOST_CA" "$tmp/root/etc/ssl/certs/ca-certificates.crt"
    ln -sf certs/ca-certificates.crt "$tmp/root/etc/ssl/cert.pem"
    mkdir -p "$tmp/root/etc"
    cat > "$tmp/root/etc/environment" <<'ENV'
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
SSL_CERT_DIR=/etc/ssl/certs
REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
ENV
fi

echo "--> writing static /etc/resolv.conf → 10.20.255.254"
rm -f "$tmp/root/etc/resolv.conf"
cat > "$tmp/root/etc/resolv.conf" <<'RESOLV'
# Managed by crucible's rootfs build. Points at the daemon's
# DNS anycast address; the daemon's DNS proxy enforces the
# sandbox's allowlist before forwarding to upstream.
nameserver 10.20.255.254
options edns0
RESOLV

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
# SSL_CERT_FILE + siblings so the agent's exec'd children (python,
# curl, node, etc.) find the CA bundle we installed. systemd doesn't
# auto-source /etc/environment for services; setting them on the
# unit is the definitive path and survives any rewrite of /etc.
Environment=SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
Environment=SSL_CERT_DIR=/etc/ssl/certs
Environment=REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
Environment=CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
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
debugfs -R "stat /etc/systemd/network/20-crucible-eth0.network" "$OUT" >/dev/null 2>&1 \
    || die "systemd-networkd eth0 config missing in output rootfs"
debugfs -R "stat /etc/systemd/system/multi-user.target.wants/systemd-networkd.service" "$OUT" >/dev/null 2>&1 \
    || die "systemd-networkd not enabled in output rootfs"
debugfs -R "stat /etc/resolv.conf" "$OUT" >/dev/null 2>&1 \
    || die "static /etc/resolv.conf missing in output rootfs"
debugfs -R "stat /etc/ssl/certs/ca-certificates.crt" "$OUT" >/dev/null 2>&1 \
    || die "CA certificate bundle missing in output rootfs"

ls -lh "$OUT"
echo "==> ok: $OUT"
