//go:build !linux

package main

import (
	"fmt"
	"io"
	"runtime"
)

// runDaemon is the non-Linux stub for the `crucible daemon` subcommand.
//
// The daemon requires KVM + Firecracker + jailer, which exist only on Linux,
// so on macOS/Windows the client can't host a daemon. It can still drive a
// *remote* Linux daemon: every other crucible command is a plain HTTP client.
// This stub keeps the single `crucible` binary buildable everywhere and, if
// someone runs `crucible daemon` on the wrong OS, tells them exactly what to
// do instead. It never touches the Linux-only daemon packages.
func runDaemon(_ []string, _, stderr io.Writer) int {
	fmt.Fprintf(stderr,
		"crucible daemon runs on Linux only (it needs KVM + Firecracker); this is %s/%s.\n\n"+
			"Run the daemon on a Linux host, then point this client at it:\n"+
			"    crucible --addr <host:7878> --token <token> sandbox ls\n"+
			"    # or set CRUCIBLE_ADDR / CRUCIBLE_TOKEN in your environment\n\n"+
			"See the README's platform matrix for the recommended remote-daemon setup.\n",
		runtime.GOOS, runtime.GOARCH)
	return 2
}
