#!/usr/bin/env bash
#
# Short diagnostic: bring up one sandbox with network, dump
# everything about its resolver + routing + DNS state to stdout.
# Use to figure out WHY example.com resolution fails inside the
# guest even though the daemon's DNS proxy is running.
#
# Usage (same env vars as smoke_e2e.sh):
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/.../firecracker \
#        JAILER_BIN=/.../jailer \
#        KERNEL=/.../vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        scripts/debug_dns.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-./assets/rootfs-with-agent.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7881}"
BASE_URL="http://${LISTEN}"
EGRESS_IFACE="${EGRESS_IFACE:-$(ip -4 route show default 2>/dev/null | awk '/default/{print $5; exit}')}"

OUT="/tmp/crucible-dns-debug-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"
DAEMON_LOG="$OUT/daemon.log"

[[ $EUID -ne 0 ]] && { echo "must run as root" >&2; exit 2; }

FRAME="$OUT/split_frames.py"
cat > "$FRAME" <<'PY'
#!/usr/bin/env python3
import json, os, struct, sys
def main(outdir):
    os.makedirs(outdir, exist_ok=True)
    data = sys.stdin.buffer.read()
    out = {1: open(os.path.join(outdir, "stdout"), "wb"),
           2: open(os.path.join(outdir, "stderr"), "wb")}
    exit_body = None
    off = 0
    while off + 8 <= len(data):
        typ = data[off]
        size = struct.unpack(">I", data[off+4:off+8])[0]
        body = data[off+8:off+8+size]
        off += 8 + size
        if typ in out: out[typ].write(body)
        elif typ == 3: exit_body = body
    for f in out.values(): f.close()
    if exit_body is not None:
        with open(os.path.join(outdir, "exit.json"), "wb") as f:
            f.write(json.dumps(json.loads(exit_body), indent=2).encode())
            f.write(b"\n")

if __name__ == "__main__":
    main(sys.argv[1])
PY
chmod +x "$FRAME"

echo "== starting daemon"
"$CRUCIBLE_BIN" daemon \
  --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" \
  --jailer-bin "$JAILER_BIN" \
  --chroot-base "$CHROOT_BASE" \
  --kernel "$KERNEL" \
  --rootfs "$ROOTFS" \
  --work-base "$WORK_BASE" \
  --network-egress-iface "$EGRESS_IFACE" \
  --log-format json --log-level info \
  >"$DAEMON_LOG" 2>&1 &
PID=$!

cleanup() {
  kill -TERM "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
}
trap cleanup EXIT

for _ in {1..100}; do
  curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done

echo "== creating sandbox with network=[example.com]"
CREATE_RESP="$OUT/create.resp.json"
curl -sS -o "$CREATE_RESP" -X POST "$BASE_URL/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["example.com"]}}'

SBX=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["id"])' "$CREATE_RESP")
GUEST_IP=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["network"]["guest_ip"])' "$CREATE_RESP")
GATEWAY=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["network"]["gateway"])' "$CREATE_RESP")
echo "   sandbox: $SBX"
echo "   guest_ip: $GUEST_IP"
echo "   gateway:  $GATEWAY"

sleep 3

exec_dump() {
  local tag="$1"
  local cmd_json="$2"
  local dir="$OUT/$tag"
  mkdir -p "$dir"
  curl -sS -o "$dir/raw.bin" -X POST "$BASE_URL/sandboxes/$SBX/exec" \
    -H 'Content-Type: application/json' -d "$cmd_json"
  "$FRAME" "$dir" < "$dir/raw.bin"
  echo ""
  echo "----- $tag -----"
  echo "[stdout]"
  cat "$dir/stdout" 2>/dev/null | head -40
  echo "[stderr]"
  cat "$dir/stderr" 2>/dev/null | head -20
  echo "[exit]"
  cat "$dir/exit.json" 2>/dev/null
}

NETNS="crucible-${SBX//_/-}"
echo ""
echo "== host-side diagnostics (netns=$NETNS)"
{
  echo "--- nft base table (root netns) ---"
  nft list table inet crucible 2>&1 || true
  echo
  echo "--- root netns: ip -4 addr (veth-h, crucible-dns) ---"
  ip -4 addr 2>&1 | grep -A1 -E 'vh-|crucible-dns' || true
  echo
  echo "--- root netns: iptables legacy count ---"
  iptables -S 2>/dev/null | head -5 || true
  echo
  echo "--- sysctl bridge-nf-call-iptables ---"
  sysctl net.bridge.bridge-nf-call-iptables 2>&1 || true
  sysctl net.ipv4.ip_forward 2>&1 || true
  echo
  echo "--- sandbox netns: ip -4 addr ---"
  ip netns exec "$NETNS" ip -4 addr 2>&1 || true
  echo
  echo "--- sandbox netns: ip -4 route ---"
  ip netns exec "$NETNS" ip -4 route 2>&1 || true
  echo
  echo "--- sandbox netns: bridge link ---"
  ip netns exec "$NETNS" bridge link 2>&1 || true
  echo
  echo "--- sandbox netns: bridge fdb ---"
  ip netns exec "$NETNS" bridge fdb 2>&1 || true
  echo
  echo "--- sandbox netns: ip neigh ---"
  ip netns exec "$NETNS" ip neigh 2>&1 || true
  echo
  echo "--- host: ping guest $GUEST_IP from root netns ---"
  ping -c2 -W2 "$GUEST_IP" 2>&1 | head -6 || true
} > "$OUT/host_diag.txt" 2>&1
cat "$OUT/host_diag.txt"

exec_dump "01_eth0_addr"      '{"cmd":["/sbin/ip","-4","addr","show","eth0"]}'
exec_dump "02_default_route"  '{"cmd":["/sbin/ip","-4","route","show"]}'
exec_dump "03_resolv_conf"    '{"cmd":["/bin/cat","/etc/resolv.conf"]}'
exec_dump "04_resolv_symlink" '{"cmd":["/bin/readlink","-f","/etc/resolv.conf"]}'
exec_dump "05_resolvectl"     '{"cmd":["/bin/sh","-c","command -v resolvectl >/dev/null && resolvectl status eth0 2>&1 || echo resolvectl not present"]}'
exec_dump "06_sd_resolved"    '{"cmd":["/bin/sh","-c","systemctl is-active systemd-resolved 2>&1 || true"]}'
exec_dump "07_sd_networkd"    '{"cmd":["/bin/sh","-c","systemctl is-active systemd-networkd 2>&1 || true"]}'
exec_dump "08_netplan_cfg"    '{"cmd":["/bin/sh","-c","ls -la /etc/netplan/ 2>&1 && echo --- && cat /etc/netplan/*.yaml 2>&1"]}'
exec_dump "09_ping_gw"        "{\"cmd\":[\"/bin/sh\",\"-c\",\"ping -c1 -W2 $GATEWAY 2>&1 | head -5\"]}"
exec_dump "10_ping_anycast"   '{"cmd":["/bin/sh","-c","ping -c1 -W2 10.20.255.254 2>&1 | head -5"]}'
exec_dump "11_dns_direct"     '{"cmd":["/bin/sh","-c","getent hosts example.com 2>&1; echo exit=$?"]}'
exec_dump "12_dig_anycast"    '{"cmd":["/bin/sh","-c","command -v dig >/dev/null && dig +short @10.20.255.254 example.com 2>&1 || echo dig not present"]}'
exec_dump "13_arp_eth0"       '{"cmd":["/bin/sh","-c","ip neigh show 2>&1; echo ---; arp -n 2>&1 || true"]}'

echo ""
echo "== deleting sandbox"
curl -sS -o /dev/null -X DELETE "$BASE_URL/sandboxes/$SBX"

echo ""
echo "== daemon log tail"
tail -40 "$DAEMON_LOG"

echo ""
echo "outputs: $OUT"
