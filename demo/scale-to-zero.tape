# cruciblehq.dev hero — v0.5.0 "scale to zero", one take: deploy a durable app
# reached by name, let it AUTO-SLEEP when idle (freeing its RAM), then watch the
# next request wake it in place — same address, sub-second — with no wake call.
#
# Render — vhs runs as your normal USER against the already-running `crucible`
# service (which the installer configures with the ingress proxy on :7879 and
# --proxy-domain apps.local). vhs itself must NOT run under sudo (its headless
# Chrome refuses to run as root); the daemon is the systemd service, not started
# here. One-time host entry so the on-screen curl reads clean:
#
#     echo '127.0.0.1 web.apps.local' | sudo tee -a /etc/hosts   # once
#     vhs demo/scale-to-zero.tape                                 # from the repo root
#
# Assumes: the service listens on 127.0.0.1:7878 (default), the proxy on :7879,
# --proxy-domain apps.local, and --network-egress-iface set. If your proxy domain
# differs, replace web.apps.local below. Non-default proxy domains: check
# /etc/crucible/crucible.env (CRUCIBLE_FLAGS --proxy-domain / --proxy-listen).
#   Cleanup when done: `sudo sed -i '/web.apps.local/d' /etc/hosts`.
#   Then: cp demo/scale-to-zero.gif ../cruciblehq/public/demo/scale-to-zero.gif
Output demo/scale-to-zero.gif

Set Shell "bash"
Set FontSize 26
Set Width 1150
Set Height 860
Set Padding 20
Set TypingSpeed 22ms
Set WaitTimeout 90s
# Match the landing page: pure black, warm-white text, forge orange heat.
Set Theme { "name": "forge", "background": "#000000", "foreground": "#f0efed", "cursor": "#ff6a1f", "black": "#101010", "red": "#ff6a1f", "green": "#8fbf6f", "yellow": "#ffb25e", "blue": "#a1a1a1", "magenta": "#ff8a3d", "cyan": "#ffb25e", "white": "#f0efed", "brightBlack": "#6e6e6e", "brightRed": "#ff8a3d", "brightGreen": "#a5d18a", "brightYellow": "#ffd9a8", "brightBlue": "#c9c9c9", "brightMagenta": "#ffb25e", "brightCyan": "#ffd9a8", "brightWhite": "#ffffff" }
# Frame the terminal as a floating app window: a macOS-style title bar with
# traffic-light dots, a dark forge-tinted backdrop, and rounded corners. Comment
# these four lines out to go back to a bare terminal.
Set Margin 34
Set MarginFill "#160d08"
Set BorderRadius 14
Set WindowBar Colorful
Set WindowBarSize 42

Hide
Type 'export PATH="$PWD:$PATH"'  Enter
Type 'D=$(mktemp -d); echo "hello from the crucible" > "$D/index.html"'  Enter
Type 'serves() { for _ in $(seq 1 60); do curl -s --max-time 5 web.apps.local:7879 2>/dev/null | grep -qi nginx && return 0; sleep 0.5; done; return 1; }'  Enter
# Clean slate + warm the image cache so app-create boots from a cached image
# rather than pulling on camera.
Type 'crucible app rm web >/dev/null 2>&1 || true'  Enter
Type 'WARM=$(crucible run nginx:alpine --pull missing 2>/dev/null); crucible rm "$WARM" >/dev/null 2>&1 || true'  Enter
Wait
Type 'clear'  Enter
Show
Sleep 400ms

Type "# deploy a durable app, reached by name — it sleeps itself when idle"  Enter
Sleep 400ms
Type "crucible app create web --image nginx:alpine --port 80 --idle-timeout 3s --min-scale 0"  Enter
Wait
Sleep 1.5s
# (hidden) wait for readiness, seed a one-line page into the instance, then
# `clear` so none of this plumbing is on screen when capture resumes — `Hide`
# only stops frame capture, the shell still echoes what's typed.
Hide
Type 'serves >/dev/null 2>&1'  Enter
Wait
Type 'SBX=$(crucible app get web | python3 -c "import json,sys;print(json.load(sys.stdin)[\"status\"][\"instance_id\"])")'  Enter
Wait
Type 'crucible cp "$D/index.html" "$SBX:/usr/share/nginx/html" >/dev/null 2>&1'  Enter
Wait
Type 'clear'  Enter
Show
Type "curl -s web.apps.local:7879"  Enter
Wait
Sleep 900ms

# With --idle-timeout 3s and the fixed 5s idle scan, the app is asleep within 8s
# of the last request — so a visible `sleep 9` lands on `asleep` every render,
# no hidden poll (which would echo onto the clean screen) required.
Type "# no traffic — crucible sleeps it on its own once idle"  Enter
Sleep 400ms
Type "sleep 9"  Enter
Wait
Type "crucible app ls"  Enter
Wait
Sleep 1.2s

Type "# you never call wake — the next request does it, in place, sub-second"  Enter
Sleep 400ms
Type "curl -s -w '\n↑ woke + served in %{time_total}s\n' web.apps.local:7879"  Enter
Wait
Sleep 700ms
Type "crucible app ls"  Enter
Wait
Sleep 3.5s

Hide
Type 'crucible app rm web >/dev/null 2>&1; rm -rf "$D"'  Enter
Wait
