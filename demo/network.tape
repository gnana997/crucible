# crucible network demo — default-deny egress + hostname allowlist.
# Render with: vhs demo/network.tape   (needs a daemon with networking enabled)
Output demo/network.gif

Set Shell "bash"
Set FontSize 16
Set Width 1240
Set Height 620
Set Padding 18
Set Theme "Aardvark Blue"
Set TypingSpeed 26ms

# use the local build as `crucible` without the ./ prefix
Hide
Type 'export PATH="$PWD:$PATH"; clear'  Enter
Show
Sleep 400ms

Type "# a crucible sandbox has NO network unless you allowlist hosts — and only those"  Enter
Sleep 700ms
Type "SBX=$(crucible sandbox create --net-allow pypi.org); echo created $SBX"  Enter
Sleep 3.6s

Type "# allowlisted host: DNS resolves…"  Enter
Sleep 350ms
Type "crucible sandbox exec $SBX -- getent hosts pypi.org"  Enter
Sleep 1.5s

Type "# …and the HTTPS request goes through:"  Enter
Sleep 350ms
Type `crucible sandbox exec $SBX -- python3 -c "import urllib.request as u; print('pypi.org HTTPS ->', u.urlopen('https://pypi.org/simple/', timeout=8).status)"`  Enter
Sleep 2s

Type "# every other host is denied by default — the name won't even resolve:"  Enter
Sleep 350ms
Type "crucible sandbox exec $SBX -- getent hosts example.com || echo 'example.com -> DNS refused (default-deny)'"  Enter
Sleep 1.9s

Type "crucible sandbox rm $SBX"  Enter
Sleep 1.3s
