# cruciblehq.dev hero — one take, the whole story: boot nginx from an OCI
# image with a published port, push content in with cp, snapshot the RUNNING
# server, fork it, diverge the fork's content, and show the two VMs serving
# different answers without touching each other.
#
# Render with: vhs demo/landing.tape   (needs a running daemon with
# --image-dir; run `crucible run nginx:alpine` once first so the image is
# cached and the recording shows warm boot speed — then `crucible rm` it)
# Then: cp demo/landing.gif ../cruciblehq/public/demo/landing.gif
Output demo/landing.gif

Set Shell "bash"
Set FontSize 26
Set Width 1150
Set Height 860
Set Padding 20
Set TypingSpeed 22ms
Set WaitTimeout 60s
# Match the landing page: pure black, warm-white text, forge orange heat.
Set Theme { "name": "forge", "background": "#000000", "foreground": "#f0efed", "cursor": "#ff6a1f", "black": "#101010", "red": "#ff6a1f", "green": "#8fbf6f", "yellow": "#ffb25e", "blue": "#a1a1a1", "magenta": "#ff8a3d", "cyan": "#ffb25e", "white": "#f0efed", "brightBlack": "#6e6e6e", "brightRed": "#ff8a3d", "brightGreen": "#a5d18a", "brightYellow": "#ffd9a8", "brightBlue": "#c9c9c9", "brightMagenta": "#ffb25e", "brightCyan": "#ffd9a8", "brightWhite": "#ffffff" }

Hide
Type 'export PATH="$PWD:$PATH"'  Enter
Type 'mkdir -p /tmp/v1 /tmp/v2 && echo "hello from the crucible" > /tmp/v1/index.html && echo "hello from the fork" > /tmp/v2/index.html && clear'  Enter
Show
Sleep 400ms

Type "# boot nginx in its own microVM, publish a port"  Enter
Sleep 400ms
Type "SBX=$(crucible run nginx:alpine -p 8080:80)"  Enter
Wait
Sleep 300ms
Type "crucible cp /tmp/v1/index.html $SBX:/usr/share/nginx/html"  Enter
Wait
Sleep 300ms
Type "curl -s localhost:8080"  Enter
Wait
Sleep 900ms

Type "# snapshot the RUNNING server, fork it onto its own port"  Enter
Sleep 400ms
Type "FORK=$(crucible snapshot create $SBX | xargs crucible fork -p 8081:80)"  Enter
Wait
Sleep 300ms

Type "# diverge the fork; the original never notices"  Enter
Sleep 400ms
Type "crucible cp /tmp/v2/index.html $FORK:/usr/share/nginx/html"  Enter
Wait
Sleep 300ms
Type "curl -s localhost:8080 && curl -s localhost:8081"  Enter
Wait
Sleep 3.5s

Hide
Type `curl -s localhost:7878/sandboxes | python3 -c 'import json,sys;[print(x["id"]) for x in json.load(sys.stdin)["sandboxes"]]' | xargs -r -n1 crucible rm; rm -rf /tmp/v1 /tmp/v2`  Enter
Wait
