# crucible build demo — build a repo's Dockerfile and run it in two lines.
# Render with: vhs demo/build.tape   (needs a daemon with --image-dir, docker, a NIC)
Output demo/build.gif

Set Shell "bash"
Set FontSize 16
Set Width 1240
Set Height 510
Set Padding 18
Set Theme "Aardvark Blue"
Set TypingSpeed 26ms

Hide
Type 'export PATH="$PWD:$PATH"; clear'  Enter
Show
Sleep 400ms

Type "# a repo with a Dockerfile:"  Enter
Sleep 400ms
Type "cat demo/app/Dockerfile"  Enter
Sleep 1.6s

Type "# build it straight into crucible's image store…"  Enter
Sleep 500ms
Type "IMG=$(crucible build -t demo-app demo/app)"  Enter
Sleep 6s
Type 'echo "built $IMG"'  Enter
Sleep 700ms

Type "# …and run it, publishing its port — two lines from Dockerfile to serving"  Enter
Sleep 500ms
Type "SBX=$(crucible run demo-app -p 8081:80)"  Enter
Sleep 6s

Type "curl -s localhost:8081"  Enter
Sleep 1.8s

Type "crucible rm $SBX"  Enter
Sleep 1.6s
