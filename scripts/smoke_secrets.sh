#!/usr/bin/env bash
#
# smoke_secrets.sh — encrypted secret bundles (v0.7.4).
#
# Proves the two halves of the feature: the secret REACHES the guest service via
# env (functional), and its plaintext value appears on NO host-visible surface
# (security — the whole point vs plaintext --env).
#
#   01  daemon up with secrets enabled (--secrets-key-file generates a key)
#   02  a secret bundle is created from a .env; `secret ls` shows the bundle +
#       its key NAMES, never values
#   03  an app is created with --secrets <bundle> (envFrom) whose service prints
#       its own env, and runs
#   04  the SERVICE's env (read from its logs) contains the secret's key=value —
#       it actually reached the guest
#   05  `app get` carries only the bundle name in secret_env_from — no value
#   06  GET /secrets and /secrets/{name} return names/keys only — no value
#   07  `admin backup` contains no plaintext value (secrets.db rides it as
#       ciphertext)
#   08  the on-disk secrets.db is ciphertext — the value is not in the file
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, python3, a pullable alpine.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_secrets.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7920}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-secrets}"
VALUE="s3cr3t-value-xyz-4242"   # the distinctive plaintext we hunt for everywhere

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible encrypted secrets smoke (v0.7.4)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v curl >/dev/null && command -v python3 >/dev/null || { echo "error: curl + python3 needed" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
SECRETS_DB="$MOUNT/secrets.db"
ENV_FILE="$MOUNT/app.env"
cat > "$ENV_FILE" <<EOF
# a .env with secrets + non-secrets mixed (the whole file becomes one bundle)
SMOKE_SECRET=$VALUE
DB_URL=postgres://user:pw@db/app
export LOG_LEVEL="debug"
EOF

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 01 start daemon with secrets enabled"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --app-db "$MOUNT/apps.db" --network-egress-iface "$EGRESS" \
  --secrets-key-file "$MOUNT/secrets.key" --secrets-db "$SECRETS_DB" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3; }
grep -q "secrets enabled" "$DAEMON_LOG" && ok "daemon up, secrets enabled" || bad "secrets not enabled"

cli() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase() { local want="$2" tries="${3:-240}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }

echo "== 02 create a secret bundle from a .env (names visible, values not)"
cli secret set web-env --from-env-file "$ENV_FILE" >/dev/null 2>&1 && ok "bundle 'web-env' stored from .env" || bad "secret set failed"
cli secret ls 2>/dev/null | grep -qx web-env && ok "'secret ls' shows the bundle name" || bad "bundle not listed"
keys="$(cli secret ls web-env 2>/dev/null)"
{ echo "$keys" | grep -qx SMOKE_SECRET && echo "$keys" | grep -qx LOG_LEVEL; } && ok "'secret ls web-env' shows key names" || bad "key names not listed"
echo "$keys" | grep -q "$VALUE" && bad "SECURITY: 'secret ls' leaked a value" || ok "'secret ls' shows no values"

echo "== 03 create an app that injects the bundle (envFrom) and dumps its env"
# Override the image entrypoint with a tiny service that prints its own
# environment to stdout (captured by the log store) and stays alive. This reads
# the SERVICE's env directly — the reliable check: an `exec` would see the agent's
# env, not the service's, and a daemon like nginx overwrites its /proc/environ
# (setproctitle).
cerr="$(cli app create web --image nginx:alpine --pull missing --restart always \
  --vcpus 1 --memory 256 --secrets web-env -- sh -c 'env; sleep 999999' 2>&1)" \
  || { bad "app create failed: $cerr"; tail -20 "$DAEMON_LOG"; exit 1; }
if wait_phase web running; then
  ok "app 'web' running"
else
  bad "app never ran (phase=$(app_phase web))"; cli app get web 2>&1 | head -c 500; echo; tail -20 "$DAEMON_LOG"; exit 1
fi

echo "== 04 the secret reached the guest SERVICE env (via its logs)"
sleep 2  # let the log drain flush the startup env dump
svc_log="$(cli app logs web --source service 2>/dev/null)"
echo "$svc_log" | grep -qx "SMOKE_SECRET=$VALUE" && ok "guest service env carries SMOKE_SECRET=<value>" \
  || bad "secret did not reach the guest service env"
echo "$svc_log" | grep -qx "LOG_LEVEL=debug" && ok "the whole bundle injected (LOG_LEVEL too)" || bad "bundle not fully injected"

echo "== 05 'app get' carries only the bundle name, no value"
appjson="$(cli app get web -o json 2>/dev/null)"
echo "$appjson" | grep -q "$VALUE" && bad "SECURITY: 'app get' leaked the secret value" || ok "'app get' has no secret value"
echo "$appjson" | grep -q "web-env" && ok "'app get' shows secret_env_from=[web-env]" || bad "secret_env_from missing from app get"

echo "== 06 the API never returns a value"
{ curl -s "$BASE_URL/secrets"; curl -s "$BASE_URL/secrets/web-env"; } | grep -q "$VALUE" \
  && bad "SECURITY: GET /secrets leaked a value" || ok "GET /secrets + /secrets/{name} return no values"

echo "== 07 'admin backup' contains no plaintext value"
curl -s "$BASE_URL/admin/backup" -o "$MOUNT/backup.tar.gz" 2>/dev/null
if [[ -s "$MOUNT/backup.tar.gz" ]]; then
  # Decompress + scan the whole tar for the plaintext; secrets.db rides as ciphertext.
  if gunzip -c "$MOUNT/backup.tar.gz" 2>/dev/null | grep -aq "$VALUE"; then
    bad "SECURITY: admin backup contains the plaintext secret value"
  else
    ok "admin backup carries the secret store as ciphertext (no plaintext value)"
  fi
else
  bad "admin backup produced no archive"
fi

echo "== 08 the on-disk secrets.db is ciphertext"
grep -aq "$VALUE" "$SECRETS_DB" && bad "SECURITY: the on-disk secrets.db contains the plaintext value" \
  || ok "on-disk secrets.db is ciphertext (no plaintext value)"

cli app rm web >/dev/null 2>&1 || true

echo "==============================================================="
echo " secrets smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
