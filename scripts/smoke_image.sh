#!/usr/bin/env bash
#
# Unprivileged end-to-end smoke for the OCI image pipeline.
#
# No KVM, no sudo, no VM boot: this drives the daemon's /images API and
# validates the converted ext4 artifacts on disk with fsck + debugfs.
# Booting images is covered by the boot smokes; this proves conversion end-to-end through the
# daemon.
#
# Scenarios:
#   01  daemon starts with --image-dir (dummy VM paths; nothing booted)
#   02  pull alpine → converted, fsck-clean, injected agent + run.json present
#   03  run.json inside the image carries the image's entrypoint/cmd
#   04  pull the same ref again → deduped (one artifact, same digest)
#   05  pull nginx → larger image converts and validates
#   06  image ls shows both; inspect returns the digest's details
#   07  import a docker-save archive (skipped if docker is absent)
#   08  image rm removes the artifact from disk and the list
#
# Requires: crucible built with an embedded agent (make build), plus
# e2fsprogs (mkfs.ext4/fsck.ext4/debugfs) and network access to
# docker.io. Docker is optional (only scenario 07 uses it).
#
# Usage:
#   make build
#   scripts/smoke_image.sh
#   # or point at a prebuilt binary:
#   CRUCIBLE_BIN=./crucible scripts/smoke_image.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
LISTEN="${LISTEN:-127.0.0.1:7882}"
BASE_URL="http://${LISTEN}"

SMOKE_ROOT="/tmp/crucible-smoke-image-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible OCI image smoke (unprivileged)"
echo "==============================================================="
echo " output dir  : $SMOKE_ROOT"
echo " crucible    : $CRUCIBLE_BIN"
echo " image dir   : $IMAGE_DIR"
echo " listen      : $LISTEN"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (run make build)" >&2; exit 2; }
for tool in mkfs.ext4 fsck.ext4 debugfs curl; do
  command -v "$tool" >/dev/null || { echo "error: $tool not installed" >&2; exit 2; }
done

jpath() {
  local file="$1"; shift
  python3 -c "
import json,sys
d = json.load(open('$file'))
for k in sys.argv[1:]:
    d = d[int(k)] if k.isdigit() else d[k]
print(d)
" "$@"
}

PASS=0
FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }

# ---- dummy VM prerequisites (never executed: no sandbox is created) ---------

FAKE_FC="$SMOKE_ROOT/fake-firecracker"
FAKE_JAILER="$SMOKE_ROOT/fake-jailer"
FAKE_KERNEL="$SMOKE_ROOT/fake-vmlinux"
FAKE_ROOTFS="$SMOKE_ROOT/fake-rootfs.ext4"
printf '#!/bin/sh\nexit 0\n' > "$FAKE_FC"; chmod +x "$FAKE_FC"
cp "$FAKE_FC" "$FAKE_JAILER"
echo dummy > "$FAKE_KERNEL"
echo dummy > "$FAKE_ROOTFS"

# ---- daemon lifecycle -------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  echo "== 01 starting daemon with --image-dir"
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FAKE_FC" \
    --kernel "$FAKE_KERNEL" \
    --rootfs "$FAKE_ROOTFS" \
    --work-base "$WORK_BASE" --app-db "$WORK_BASE-apps.db" \
    --image-dir "$IMAGE_DIR" \
    --log-format json --log-level info \
    >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..50}; do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
      pass "daemon healthy with image support"
      return 0
    fi
    kill -0 "$DAEMON_PID" 2>/dev/null || { fail "daemon exited early"; tail -20 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  fail "daemon never became healthy"; tail -20 "$DAEMON_LOG"; exit 3
}

cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

cli() { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# find the on-disk rootfs.ext4 for a given short digest (first 12 hex).
rootfs_for() {
  local short="$1"
  ls -d "$IMAGE_DIR"/sha256_"$short"* 2>/dev/null | head -1 | sed 's:$:/rootfs.ext4:'
}

# assert a converted image is valid: fsck clean + injected files present.
assert_image_valid() {
  local rootfs="$1" label="$2"
  if [[ ! -f "$rootfs" ]]; then
    fail "$label: rootfs.ext4 not found on disk"
    return
  fi
  if fsck.ext4 -f -n "$rootfs" >/dev/null 2>&1; then
    pass "$label: fsck clean"
  else
    fail "$label: fsck reported problems"
  fi
  local agent_stat
  agent_stat="$(debugfs -R 'stat /crucible/crucible-agent' "$rootfs" 2>/dev/null)"
  if echo "$agent_stat" | grep -q "Inode:"; then
    pass "$label: /crucible/crucible-agent present"
  else
    fail "$label: injected agent missing"
  fi
  if debugfs -R 'stat /crucible/run.json' "$rootfs" 2>/dev/null | grep -q "Inode:"; then
    pass "$label: /crucible/run.json present"
  else
    fail "$label: injected run.json missing"
  fi
}

start_daemon

# ---- 02 pull alpine ---------------------------------------------------------

echo "== 02 pull alpine and validate the artifact"
ALPINE_JSON="$SMOKE_ROOT/alpine.json"
if cli image pull alpine:latest -o json > "$ALPINE_JSON" 2>"$SMOKE_ROOT/alpine.err"; then
  ALPINE_DIGEST="$(jpath "$ALPINE_JSON" digest)"
  pass "pulled alpine ($ALPINE_DIGEST)"
  ALPINE_SHORT="${ALPINE_DIGEST#sha256:}"; ALPINE_SHORT="${ALPINE_SHORT:0:12}"
  assert_image_valid "$(rootfs_for "$ALPINE_SHORT")" "alpine"
else
  fail "pull alpine: $(cat "$SMOKE_ROOT/alpine.err")"
  echo "   (network access to docker.io required)"
fi

# ---- 03 run.json content ----------------------------------------------------

echo "== 03 run.json inside the image records the runtime contract"
if [[ -n "${ALPINE_SHORT:-}" ]]; then
  ROOTFS="$(rootfs_for "$ALPINE_SHORT")"
  debugfs -R 'cat /crucible/run.json' "$ROOTFS" > "$SMOKE_ROOT/alpine-run.json" 2>/dev/null
  if python3 -c "
import json
d = json.load(open('$SMOKE_ROOT/alpine-run.json'))
assert d['version'] == 1, d
# alpine's default cmd is /bin/sh
assert d.get('cmd') or d.get('entrypoint'), d
print('ok')
" >/dev/null 2>&1; then
    pass "run.json is valid v1 with a command"
  else
    fail "run.json invalid: $(cat "$SMOKE_ROOT/alpine-run.json" 2>/dev/null | head -c 200)"
  fi
else
  echo "   SKIP: alpine pull did not succeed"
fi

# ---- 04 dedup ---------------------------------------------------------------

echo "== 04 re-pull alpine dedupes"
if [[ -n "${ALPINE_DIGEST:-}" ]]; then
  BEFORE="$(ls -d "$IMAGE_DIR"/sha256_* 2>/dev/null | wc -l)"
  cli image pull alpine:latest -o json >/dev/null 2>&1
  AFTER="$(ls -d "$IMAGE_DIR"/sha256_* 2>/dev/null | wc -l)"
  if [[ "$BEFORE" == "$AFTER" ]]; then
    pass "re-pull produced no new artifact ($AFTER on disk)"
  else
    fail "dedup failed: $BEFORE → $AFTER artifacts"
  fi
else
  echo "   SKIP"
fi

# ---- 05 pull nginx ----------------------------------------------------------

echo "== 05 pull nginx (larger image)"
NGINX_JSON="$SMOKE_ROOT/nginx.json"
if cli image pull nginx:latest -o json > "$NGINX_JSON" 2>"$SMOKE_ROOT/nginx.err"; then
  NGINX_DIGEST="$(jpath "$NGINX_JSON" digest)"
  NGINX_SHORT="${NGINX_DIGEST#sha256:}"; NGINX_SHORT="${NGINX_SHORT:0:12}"
  pass "pulled nginx ($NGINX_DIGEST)"
  assert_image_valid "$(rootfs_for "$NGINX_SHORT")" "nginx"
else
  fail "pull nginx: $(cat "$SMOKE_ROOT/nginx.err")"
fi

# ---- 06 ls + inspect --------------------------------------------------------

echo "== 06 image ls + inspect"
if cli image ls | grep -q "alpine\|nginx"; then
  pass "image ls shows converted images"
else
  fail "image ls empty or wrong: $(cli image ls)"
fi
if [[ -n "${ALPINE_DIGEST:-}" ]]; then
  if cli image inspect "$ALPINE_DIGEST" -o json | grep -q '"digest"'; then
    pass "image inspect returns details"
  else
    fail "image inspect"
  fi
fi

# ---- 07 import a docker-save archive (docker optional) ----------------------

echo "== 07 import a docker-save archive"
if command -v docker >/dev/null 2>&1; then
  ARCHIVE="$SMOKE_ROOT/busybox.tar"
  if docker pull busybox:latest >/dev/null 2>&1 && docker save busybox:latest -o "$ARCHIVE" 2>/dev/null; then
    if cli image import --file "$ARCHIVE" -o json > "$SMOKE_ROOT/import.json" 2>"$SMOKE_ROOT/import.err"; then
      IMPORT_DIGEST="$(jpath "$SMOKE_ROOT/import.json" digest)"
      IMPORT_SHORT="${IMPORT_DIGEST#sha256:}"; IMPORT_SHORT="${IMPORT_SHORT:0:12}"
      pass "imported busybox from docker-save ($IMPORT_DIGEST)"
      assert_image_valid "$(rootfs_for "$IMPORT_SHORT")" "busybox-import"
    else
      fail "import: $(cat "$SMOKE_ROOT/import.err")"
    fi
  else
    echo "   SKIP: could not docker pull/save busybox"
  fi
else
  echo "   SKIP: docker not installed (import path exercised by unit tests)"
fi

# ---- 08 rm ------------------------------------------------------------------

echo "== 08 image rm removes the artifact"
if [[ -n "${ALPINE_DIGEST:-}" ]]; then
  DIR_BEFORE="$(rootfs_for "$ALPINE_SHORT")"
  cli image rm "$ALPINE_DIGEST" >/dev/null 2>&1
  if [[ ! -f "$DIR_BEFORE" ]] && ! cli image ls | grep -q "$ALPINE_SHORT"; then
    pass "alpine removed from disk and list"
  else
    fail "rm left artifacts behind"
  fi
else
  echo "   SKIP"
fi

# ---- summary ----------------------------------------------------------------

echo "==============================================================="
echo " image smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
