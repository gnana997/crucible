package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
)

func newSandboxCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Short:   "Manage sandboxes",
		Aliases: []string{"sbx"},
	}
	cmd.AddCommand(
		newSandboxCreateCmd(o),
		newSandboxLsCmd(o),
		newSandboxInspectCmd(o),
		newSandboxRmCmd(o),
		newSandboxExecCmd(o),
	)
	return cmd
}

func newSandboxCreateCmd(o *globalOpts) *cobra.Command {
	var (
		vcpus, memory, timeout int
		profile                string
		netAllow               []string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := api.CreateSandboxRequest{VCPUs: vcpus, MemoryMiB: memory, TimeoutSec: timeout, Profile: profile}
			if len(netAllow) > 0 {
				req.Network = &api.NetworkRequest{Enabled: true, Allowlist: netAllow}
			}
			sb, err := o.client().CreateSandbox(cmd.Context(), req)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), sb)
			}
			fmt.Fprintln(cmd.OutOrStdout(), sb.ID)
			return nil
		},
	}
	cmd.Flags().IntVar(&vcpus, "vcpus", 0, "vCPUs (0 = daemon default)")
	cmd.Flags().IntVar(&memory, "memory", 0, "memory in MiB (0 = daemon default)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "max lifetime in seconds (0 = none)")
	cmd.Flags().StringVar(&profile, "profile", "", "rootfs profile (e.g. python-3.12)")
	cmd.Flags().StringSliceVar(&netAllow, "net-allow", nil, "allowlisted hostname (repeatable); enables networking")
	return cmd
}

func newSandboxLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List sandboxes",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sbs, err := o.client().ListSandboxes(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), sbs)
			}
			tw := newTable(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tPROFILE\tVCPUS\tMEM(MiB)\tNET\tAGE")
			for _, s := range sbs {
				prof := s.Profile
				if prof == "" {
					prof = "-"
				}
				net := "-"
				if s.Network != nil && s.Network.GuestIP != "" {
					net = s.Network.GuestIP
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", s.ID, prof, s.VCPUs, s.MemoryMiB, net, age(s.CreatedAt))
			}
			return tw.Flush()
		},
	}
}

func newSandboxInspectCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "Show a sandbox's full details (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := o.client().GetSandbox(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), sb)
		},
	}
}

func newSandboxRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>...",
		Short:   "Destroy one or more sandboxes",
		Aliases: []string{"delete"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, id := range args {
				if err := cl.DeleteSandbox(cmd.Context(), id); err != nil {
					return fmt.Errorf("delete %s: %w", id, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
}

func newSandboxExecCmd(o *globalOpts) *cobra.Command {
	var (
		cwd     string
		timeout int
		env     []string
	)
	cmd := &cobra.Command{
		Use:   "exec <id> -- <command>...",
		Short: "Run a command in a sandbox and stream its output",
		Long: "Run a command in a sandbox and stream its stdout/stderr. The process " +
			"exit code becomes crucible's exit code. Use -- to separate the command " +
			"from crucible's own flags.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := agentwire.ExecRequest{Cmd: args[1:], Cwd: cwd, TimeoutSec: timeout}
			if len(env) > 0 {
				req.Env = make(map[string]string, len(env))
				for _, kv := range env {
					k, v, ok := strings.Cut(kv, "=")
					if !ok {
						return fmt.Errorf("--env %q must be KEY=VALUE", kv)
					}
					req.Env[k] = v
				}
			}
			res, err := o.client().Exec(cmd.Context(), args[0], req, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return exitCodeError{res.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory inside the guest")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "command deadline in seconds (0 = none)")
	cmd.Flags().StringArrayVar(&env, "env", nil, "environment KEY=VALUE (repeatable)")
	return cmd
}
