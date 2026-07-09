# crucible run demo — boot an unmodified OCI image, publish a port, default-deny egress.
# Render with: vhs demo/run.tape   (needs a daemon with --image-dir + a NIC)
Output demo/run.gif

Set Shell "bash"
Set FontSize 16
Set Width 1240
Set Height 510
Set Padding 18
Set Theme "Aardvark Blue"
Set TypingSpeed 26ms

# use the local build as `crucible` without the ./ prefix
Hide
Type 'export PATH="$PWD:$PATH"; clear'  Enter
Show
Sleep 400ms

Type "# boot an unmodified public image in a real microVM, and publish a port"  Enter
Sleep 700ms
Type "SBX=$(crucible run nginx:alpine -p 8080:80)"  Enter
Sleep 6s
Type 'echo "booted $SBX"'  Enter
Sleep 600ms

Type "# reach the service from the host — real ingress into the guest VM"  Enter
Sleep 400ms
Type "curl -s localhost:8080 | head -4"  Enter
Sleep 1.8s

Type "# but the guest has NO outbound network unless you allowlist it:"  Enter
Sleep 500ms
Type "crucible sandbox exec $SBX -- wget -T 3 -qO- http://example.com || echo 'egress DENIED (default-deny)'"  Enter
Sleep 3.5s

Type "# tear it down"  Enter
Sleep 300ms
Type "crucible rm $SBX"  Enter
Sleep 1.6s
