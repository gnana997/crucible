# crucible cp demo — drop local code into a sandbox and run it. No image build.
# Render with: vhs demo/cp.tape   (needs a v0.3.2 daemon; python:3.12-slim warmed)
Output demo/cp.gif

Set Shell "bash"
Set FontSize 16
Set Width 1240
Set Height 560
Set Padding 18
Set Theme "Aardvark Blue"
Set TypingSpeed 24ms

Hide
Type 'export PATH="$PWD:$PATH"; clear'  Enter
Show
Sleep 400ms

Type "# my local Python — no Dockerfile, no image build"  Enter
Sleep 500ms
Type "cat demo/analyze.py"  Enter
Sleep 2s

Type "# boot a fresh Python sandbox"  Enter
Sleep 500ms
Type "SBX=$(crucible run python:3.12-slim)"  Enter
Sleep 5s

Type "# drop my code straight in…"  Enter
Sleep 500ms
Type "crucible cp ./demo/analyze.py $SBX:/work"  Enter
Sleep 2s

Type "# …and run it in the isolated microVM"  Enter
Sleep 500ms
Type "crucible sandbox exec $SBX -- python3 /work/analyze.py"  Enter
Sleep 2.5s

Type "crucible rm $SBX"  Enter
Sleep 1.5s
