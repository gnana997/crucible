#!/usr/bin/env bash
# Seed a crucible daemon with demo state for the TUI recording (demo/tui.tape):
# a couple of sandboxes (one on the python profile), a snapshot, and a fork so
# the dashboard and the fork tree both look real. Idempotent — clears first.
#
#   ./demo/seed.sh [addr]        # default addr 127.0.0.1:7878
set -u

ADDR="${1:-127.0.0.1:7878}"
CR=(./crucible --addr "$ADDR")

ids() { # ids <sandboxes|snapshots>
  "${CR[@]}" -o json "$1" list 2>/dev/null | python3 -c \
    "import sys,json;d=json.load(sys.stdin);[print(x['id']) for x in (d if isinstance(d,list) else d.get('$1',[]))]"
}

echo "== clearing existing state =="
for id in $(ids sandbox); do "${CR[@]}" sandbox delete "$id" >/dev/null 2>&1 && echo "  - sbx $id"; done
for id in $(ids snapshot); do "${CR[@]}" snapshot delete "$id" >/dev/null 2>&1 && echo "  - snap $id"; done

echo "== creating sandboxes =="
A=$("${CR[@]}" sandbox create --profile python-3.12) && echo "  python: $A"
if B=$("${CR[@]}" sandbox create --net-allow pypi.org 2>/tmp/crucible-seed-neterr); then
  echo "  networked: $B"
else
  echo "  (networked create failed — $(tr -d '\n' </tmp/crucible-seed-neterr); using a plain sandbox)"
  B=$("${CR[@]}" sandbox create) && echo "  plain: $B"
fi

echo "== snapshot + fork (builds the tree lineage) =="
S=$("${CR[@]}" snapshot create "$A") && echo "  snapshot: $S"
C=$("${CR[@]}" fork "$S") && echo "  fork: $C"

echo "== result =="
"${CR[@]}" sandbox list
