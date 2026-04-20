// Command crucible is the entry point for the crucible sandbox runtime.
//
// v0.1 scaffolding only: subcommands are wired up but their implementations
// land in subsequent weekends. `crucible version` works today; the rest
// prints a "not yet implemented" stub so operators can see the CLI shape.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gnana997/crucible/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "crucible %s\n", version.String())
		return 0
	case "daemon":
		return runDaemon(args[1:], stdout, stderr)
	case "run":
		fmt.Fprintln(stderr, "crucible run: not yet implemented (wk2 target)")
		return 1
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `crucible - OSS sandbox runtime for AI coding agents

Usage:
  crucible <command> [flags]

Commands:
  daemon    Run the crucible HTTP daemon
  run       Run a command inside a new sandbox
  version   Print version info
  help      Show this help

See https://github.com/gnana997/crucible for docs.
`)
}
