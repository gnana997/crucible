# crucible hero — docker-run ergonomics + a k9s-style TUI.
# Render with: vhs demo/hero.tape   (needs a daemon with --image-dir + --log-dir + docker)
Output demo/hero.gif

Set Shell "bash"
Set FontSize 16
Set Width 1280
Set Height 720
Set Padding 18
Set Theme "Aardvark Blue"
Set TypingSpeed 24ms

Hide
Type 'export PATH="$PWD:$PATH"; clear'  Enter
Show
Sleep 400ms

# --- docker parity: build a Dockerfile and run it in a microVM ---
Type "# build a repo's Dockerfile straight into crucible, then run it"  Enter
Sleep 600ms
Type "crucible build -t demo-app demo/app"  Enter
Sleep 4s
Type "SBX=$(crucible run demo-app -p 8080:80)"  Enter
Sleep 6s
Type "curl -s localhost:8080"  Enter
Sleep 1.8s

# --- k9s-style TUI: watch and inspect it live ---
Type "# now open the dashboard — like k9s, for your sandboxes"  Enter
Sleep 700ms
Type "crucible tui"  Enter
Sleep 3s
# 'l' tails the selected sandbox's durable logs, live
Type "l"
Sleep 4s
Down
Down
Down
Sleep 2s
Escape
Sleep 900ms
Type "q"
Sleep 800ms
