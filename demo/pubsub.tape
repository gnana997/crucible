# crucible hero (redis use case) — v0.6.1 "scale-to-zero pub/sub": a redis that
# stays awake while a subscriber is connected and sleeps when nobody's watching,
# waking on the next connection. Connection-scoped scale-to-zero via
# --keep-connections (the forwarder never reaps a live-but-quiet subscription).
#
# Render — vhs runs as your normal USER against the already-running `crucible`
# service (the installer configures --network-egress-iface). vhs must NOT run
# under sudo. Needs redis-cli (the `redis-tools` package) on the host and
# redis:alpine pullable:
#
#     vhs demo/pubsub.tape        # from the repo root
#
#   Then: cp demo/pubsub.gif ../cruciblehq/public/demo/pubsub.gif
Output demo/pubsub.gif

Set Shell "bash"
Set FontSize 24
Set Width 1200
Set Height 860
Set Padding 20
Set TypingSpeed 22ms
Set WaitTimeout 120s
# Match the landing page: pure black, warm-white text, forge orange heat.
Set Theme { "name": "forge", "background": "#000000", "foreground": "#f0efed", "cursor": "#ff6a1f", "black": "#101010", "red": "#ff6a1f", "green": "#8fbf6f", "yellow": "#ffb25e", "blue": "#a1a1a1", "magenta": "#ff8a3d", "cyan": "#ffb25e", "white": "#f0efed", "brightBlack": "#6e6e6e", "brightRed": "#ff8a3d", "brightGreen": "#a5d18a", "brightYellow": "#ffd9a8", "brightBlue": "#c9c9c9", "brightMagenta": "#ffb25e", "brightCyan": "#ffd9a8", "brightWhite": "#ffffff" }
Set Margin 34
Set MarginFill "#160d08"
Set BorderRadius 14
Set WindowBar Colorful
Set WindowBarSize 42

Hide
Type 'export PATH="$PWD:$PATH"'  Enter
Type 'r() { redis-cli -h 127.0.0.1 "$@"; }'  Enter
Type 'ready() { for _ in $(seq 1 80); do r ping >/dev/null 2>&1 && return 0; sleep 0.5; done; return 1; }'  Enter
# Clean slate + warm the converted-image cache (`-- true` skips the server) so
# `app create` boots from a cached image rather than pulling on camera.
Type 'crucible app rm bus >/dev/null 2>&1 || true'  Enter
Type 'WARM=$(crucible run redis:alpine --pull missing -- true 2>/dev/null); crucible rm "$WARM" >/dev/null 2>&1 || true'  Enter
Wait
Type 'clear'  Enter
Show
Sleep 400ms

Type "# a serverless pub/sub redis: --keep-connections keeps it awake while subscribed"  Enter
Sleep 400ms
Type "crucible app create bus --image redis:alpine -p 6379:6379 --min-scale 0 --idle-timeout 3s --keep-connections --health tcp:6379"  Enter
Wait
Sleep 1s
Hide
Type 'ready'  Enter
Wait
Type 'clear'  Enter
Show

Type "# it's real redis on :6379"  Enter
Sleep 300ms
Type 'r ping'  Enter
Wait
Sleep 800ms

Type "# a live subscriber holds the connection open, so it stays awake even when idle"  Enter
Sleep 400ms
# Background redis-cli DIRECTLY (not via the `r` function): backgrounding a
# function runs it in a subshell, so $! would be the subshell's PID and a later
# `kill` would miss the real redis-cli child, leaving the connection (and the app)
# alive. Calling redis-cli directly makes $! the process that holds the socket.
Type 'redis-cli -h 127.0.0.1 subscribe room > /tmp/crucible-sub.log 2>&1 & SUB=$!'  Enter
Wait
Sleep 300ms
Type "sleep 7"  Enter
Wait
Type "crucible app ls   # still running: a subscriber is connected"  Enter
Wait
Sleep 1s
Type 'r publish room "hello subscribers"   # (integer) 1 = delivered'  Enter
Wait
Sleep 900ms

Type "# when the last subscriber leaves, it sleeps and frees the RAM"  Enter
Sleep 400ms
Type 'kill "$SUB" 2>/dev/null'  Enter
Wait
Type "sleep 9"  Enter
Wait
Type "crucible app ls   # asleep: nobody's connected"  Enter
Wait
Sleep 1.2s

Type "# the next connection wakes it, in place"  Enter
Sleep 400ms
Type 'r ping'  Enter
Wait
Sleep 700ms
Type "crucible app ls"  Enter
Wait
Sleep 3s

Hide
Type 'kill "$SUB" 2>/dev/null; crucible app rm bus >/dev/null 2>&1; rm -f /tmp/crucible-sub.log'  Enter
Wait
