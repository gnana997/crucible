# crucible TUI demo — render with: vhs demo/tui.tape
# Needs a daemon on 127.0.0.1:7878 pre-seeded with a few sandboxes + a fork
# (run demo/seed.sh first). https://github.com/charmbracelet/vhs
Output demo/tui.gif

Set Shell "bash"
Set FontSize 15
Set Width 1180
Set Height 700
Set Padding 16
Set Theme "Aardvark Blue"
Set TypingSpeed 32ms

# start the dashboard on an already-populated daemon
Hide
Type "./crucible tui"  Enter
Sleep 1.8s
Show
Sleep 1.2s

# browse the sandbox table
Down  Sleep 550ms
Down  Sleep 750ms
Up    Sleep 550ms

# open the detail view and run a live streaming command
Enter  Sleep 900ms
Type "echo hi; uname -sr; for i in 1 2 3; do echo tick $i; sleep 0.3; done"
Sleep 300ms
Enter  Sleep 2.4s
Escape  Sleep 700ms

# fork tree — the seeded lineage (sandbox -> snapshot -> fork)
Type "t"  Sleep 2s
Type "t"  Sleep 500ms

# live: snapshot the selected sandbox, then fork a child from it
Type "s"  Sleep 1.3s
Type "f"  Sleep 2.7s

# back to the tree — the new child is nested under its snapshot
Type "t"  Sleep 2.2s
Type "t"  Sleep 500ms

# create one more, then delete the selection (with y/n confirm)
Type "c"  Sleep 2.4s
Type "d"  Sleep 900ms
Type "y"  Sleep 2s

Sleep 700ms
Type "q"
