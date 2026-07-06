package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/version"
)

// newTable returns a tabwriter tuned for the CLI's aligned list output.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

// age renders a compact "how long ago" from a timestamp.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "crucible %s\n", version.String())
			return nil
		},
	}
}

// newDaemonCmd wraps the existing runDaemon flag-based entry point. Flag
// parsing is disabled so the daemon keeps its own flag set unchanged.
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "daemon [flags]",
		Short:              "Run the crucible HTTP daemon",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := runDaemon(args, cmd.OutOrStdout(), cmd.ErrOrStderr()); code != 0 {
				return exitCodeError{code}
			}
			return nil
		},
	}
}

func newForkCmd(o *globalOpts) *cobra.Command {
	var count int
	cmd := &cobra.Command{
		Use:   "fork <snapshot-id>",
		Short: "Fork one or more sandboxes from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			forks, err := o.client().Fork(cmd.Context(), args[0], count)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), forks)
			}
			for _, f := range forks {
				fmt.Fprintln(cmd.OutOrStdout(), f.ID)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&count, "count", 1, "number of forks to create")
	return cmd
}

func newProfileCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Inspect rootfs profiles"}
	cmd.AddCommand(&cobra.Command{
		Use:     "ls",
		Short:   "List the daemon's configured rootfs profiles",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			profs, err := o.client().ListProfiles(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), profs)
			}
			if len(profs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no profiles configured (daemon started without --rootfs-dir)")
				return nil
			}
			for _, p := range profs {
				fmt.Fprintln(cmd.OutOrStdout(), p)
			}
			return nil
		},
	})
	return cmd
}

// newRunCmd is the one-shot: create → exec (streamed) → delete. The
// sandbox is always deleted on exit unless --keep, even if exec fails.
func newRunCmd(o *globalOpts) *cobra.Command {
	var (
		vcpus, memory, timeout int
		profile                string
		netAllow               []string
		keep                   bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command>...",
		Short: "Create a sandbox, run a command in it, then delete it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			req := api.CreateSandboxRequest{VCPUs: vcpus, MemoryMiB: memory, TimeoutSec: timeout, Profile: profile}
			if len(netAllow) > 0 {
				req.Network = &api.NetworkRequest{Enabled: true, Allowlist: netAllow}
			}
			sb, err := cl.CreateSandbox(cmd.Context(), req)
			if err != nil {
				return err
			}
			if !keep {
				// Background context so cleanup runs even if cmd.Context()
				// was cancelled (e.g. Ctrl-C during exec).
				defer func() { _ = cl.DeleteSandbox(context.Background(), sb.ID) }()
			}
			res, err := cl.Exec(cmd.Context(), sb.ID, agentwire.ExecRequest{Cmd: args, TimeoutSec: timeout}, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return exitCodeError{res.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&vcpus, "vcpus", 0, "vCPUs (0 = daemon default)")
	cmd.Flags().IntVar(&memory, "memory", 0, "memory in MiB (0 = daemon default)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "command + sandbox timeout in seconds")
	cmd.Flags().StringVar(&profile, "profile", "", "rootfs profile (e.g. python-3.12)")
	cmd.Flags().StringSliceVar(&netAllow, "net-allow", nil, "allowlisted hostname (repeatable); enables networking")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox instead of deleting it after the command")
	return cmd
}
