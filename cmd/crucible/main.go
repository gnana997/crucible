// Command crucible is the CLI and daemon for the crucible sandbox
// runtime. `crucible daemon` runs the HTTP server; the other subcommands
// are a thin client over its REST API (see internal/client).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/client"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run builds the command tree and executes it, translating errors into a
// process exit code. A command may carry a specific exit code (e.g. exec
// propagating the guest command's status) via exitCodeError.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		var ec exitCodeError
		if errors.As(err, &ec) {
			return ec.code
		}
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// exitCodeError lets a command set the process exit code without printing
// an error — used by `exec`/`run` to propagate the guest command's status.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// globalOpts holds flags shared by every client subcommand.
type globalOpts struct {
	addr          string
	token         string
	output        string
	tlsSkipVerify bool
}

func (o *globalOpts) client() *client.Client {
	var opts []client.Option
	if o.token != "" {
		opts = append(opts, client.WithToken(o.token))
	}
	if o.tlsSkipVerify {
		opts = append(opts, client.WithInsecureSkipVerify())
	}
	return client.New(o.addr, opts...)
}

func (o *globalOpts) isJSON() bool { return o.output == "json" }

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := &globalOpts{}
	root := &cobra.Command{
		Use:           "crucible",
		Short:         "crucible — OSS Firecracker sandbox runtime for AI coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if opts.output != "table" && opts.output != "json" {
				return fmt.Errorf("--output must be table or json, got %q", opts.output)
			}
			return nil
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	defAddr := os.Getenv("CRUCIBLE_ADDR")
	if defAddr == "" {
		defAddr = client.DefaultAddr
	}
	root.PersistentFlags().StringVar(&opts.addr, "addr", defAddr, "daemon address (env CRUCIBLE_ADDR)")
	root.PersistentFlags().StringVar(&opts.token, "token", os.Getenv("CRUCIBLE_TOKEN"), "API key for an authenticated daemon (env CRUCIBLE_TOKEN)")
	root.PersistentFlags().StringVarP(&opts.output, "output", "o", "table", "output format: table|json")
	root.PersistentFlags().BoolVar(&opts.tlsSkipVerify, "tls-skip-verify", false, "skip TLS certificate verification (self-signed daemon; dev only)")

	root.AddCommand(
		newVersionCmd(),
		newDaemonCmd(),
		newSandboxCmd(opts),
		newShellCmd(opts),
		newServiceCmd(opts),
		newLogsCmd(opts),
		newImageCmd(opts),
		newSnapshotCmd(opts),
		newForkCmd(opts),
		newProfileCmd(opts),
		newRunCmd(opts),
		newMcpCmd(opts),
		newPolicyCmd(opts),
		newTuiCmd(opts),
	)
	return root
}
