# crucible-bench demo — render with: vhs demo/bench.tape
# Needs a daemon on 127.0.0.1:7878 (a short run; full numbers in docs/benchmarks.md).
Output demo/bench.gif

Set Shell "bash"
Set FontSize 15
Set Width 1200
Set Height 800
Set Padding 16
Set Theme "Aardvark Blue"

Hide
Type "clear" Enter
Show
Sleep 500ms
Type "./bin/crucible-bench --samples 6 --warmup 2 --fanout 1,64,128 --mem-forks 128 --density 128"
Sleep 800ms
Enter
Sleep 52s
