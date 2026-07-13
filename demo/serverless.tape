# crucible hero — v0.6.x "serverless postgres", one take: deploy a postgres on a
# durable volume that scales to zero when idle, then watch the next connection
# wake it with its data intact. Persistent volumes (v0.6.0) + wake-on-TCP (v0.6.1)
# in a single story: zero RAM when nobody's connected, a real database when they are.
#
# Render — vhs runs as your normal USER against the already-running `crucible`
# service (the installer configures --volume-dir + --network-egress-iface). vhs
# must NOT run under sudo (its headless Chrome refuses to run as root); the daemon
# is the systemd service, not started here. Needs `psql` on the host and
# postgres:16-alpine pullable (or pre-warmed in the image cache):
#
#     vhs demo/serverless.tape        # from the repo root
#
# Assumes the service listens on 127.0.0.1:7878 (default) with volumes + egress
# enabled. Cleanup is automatic (the app + volume are removed at the end).
#   Then: cp demo/serverless.gif ../cruciblehq/public/demo/serverless.gif
Output demo/serverless.gif

Set Shell "bash"
Set FontSize 24
Set Width 1200
Set Height 860
Set Padding 20
Set TypingSpeed 22ms
Set WaitTimeout 180s
# Match the landing page: pure black, warm-white text, forge orange heat.
Set Theme { "name": "forge", "background": "#000000", "foreground": "#f0efed", "cursor": "#ff6a1f", "black": "#101010", "red": "#ff6a1f", "green": "#8fbf6f", "yellow": "#ffb25e", "blue": "#a1a1a1", "magenta": "#ff8a3d", "cyan": "#ffb25e", "white": "#f0efed", "brightBlack": "#6e6e6e", "brightRed": "#ff8a3d", "brightGreen": "#a5d18a", "brightYellow": "#ffd9a8", "brightBlue": "#c9c9c9", "brightMagenta": "#ffb25e", "brightCyan": "#ffd9a8", "brightWhite": "#ffffff" }
# Frame the terminal as a floating app window: a macOS-style title bar with
# traffic-light dots, a dark forge-tinted backdrop, and rounded corners.
Set Margin 34
Set MarginFill "#160d08"
Set BorderRadius 14
Set WindowBar Colorful
Set WindowBarSize 42

Hide
Type 'export PATH="$PWD:$PATH"'  Enter
Type 'export PGPASSWORD=demo PGCONNECT_TIMEOUT=30'  Enter
Type 'psql_do() { psql -h 127.0.0.1 -U postgres "$@"; }'  Enter
Type 'ready() { for _ in $(seq 1 150); do psql_do -c "SELECT 1" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }'  Enter
# psql_q runs a single query, retrying quietly until it succeeds. A cold wake
# runs crash recovery, during which postgres rejects queries ("starting up") for
# a second or two, so the first attempt can lose the race. Retrying reads clean
# on camera (a brief pause, then the table), and mirrors the smoke's read-back.
Type 'psql_q() { for _ in $(seq 1 40); do if o="$(psql_do -c "$1" 2>/dev/null)"; then printf "%s\n" "$o"; return 0; fi; sleep 0.5; done; psql_do -c "$1"; }'  Enter
# Clean slate + warm the converted-image cache (`-- true` skips initdb) so
# `app create` boots from a cached image rather than pulling/converting on camera.
Type 'crucible app rm db >/dev/null 2>&1; crucible volume rm pgdata >/dev/null 2>&1 || true'  Enter
Type 'WARM=$(crucible run postgres:16-alpine --pull missing -- true 2>/dev/null); crucible rm "$WARM" >/dev/null 2>&1 || true'  Enter
Wait
Type 'clear'  Enter
Show
Sleep 400ms

Type "# a serverless postgres: durable volume, scales to zero when idle"  Enter
Sleep 400ms
Type "crucible app create db --image postgres:16-alpine -p 5432:5432 --volume pgdata:/var/lib/postgresql/data --min-scale 0 --idle-timeout 3s --health tcp:5432 -e POSTGRES_PASSWORD=demo -e PGDATA=/var/lib/postgresql/data/pgdata"  Enter
Wait
Sleep 1.2s

# (hidden) wait for the first-boot initdb, write a row, then `clear` so none of
# the plumbing is on screen when capture resumes.
Hide
Type 'ready'  Enter
Wait
Type 'psql_do -c "CREATE TABLE notes (msg text)" -c "INSERT INTO notes VALUES(\$\$durable data, zero RAM when idle\$\$)" >/dev/null 2>&1'  Enter
Wait
Type 'clear'  Enter
Show

Type "# connect over :5432, it's a real postgres with your data"  Enter
Sleep 400ms
Type 'psql_q "SELECT msg FROM notes"'  Enter
Wait
Sleep 1.2s

# With --idle-timeout 3s and the 5s idle scan, psql disconnected after each -c, so
# the app is asleep within ~8s: a visible `sleep 9` lands on `asleep` every render.
Type "# no connections, so crucible sleeps it and frees the RAM (the volume keeps the data)"  Enter
Sleep 400ms
Type "sleep 9"  Enter
Wait
Type "crucible app ls"  Enter
Wait
Sleep 1.4s

Type "# the next connection wakes it, data intact on the volume"  Enter
Sleep 400ms
Type 'psql_q "SELECT msg FROM notes"'  Enter
Wait
Sleep 900ms
Type "crucible app ls"  Enter
Wait
Sleep 3.5s

Hide
Type 'crucible app rm db >/dev/null 2>&1; crucible volume rm pgdata >/dev/null 2>&1 || true'  Enter
Wait
