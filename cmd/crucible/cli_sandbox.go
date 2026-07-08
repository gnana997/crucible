package main

import (
	"fmt"
	"strconv"
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
		image                  string
		pull                   string
		netAllow               []string
		publish                []string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if image != "" && profile != "" {
				return fmt.Errorf("--image and --profile are mutually exclusive")
			}
			req := api.CreateSandboxRequest{VCPUs: vcpus, MemoryMiB: memory, TimeoutSec: timeout, Profile: profile}
			if image != "" {
				ref, effPull, err := resolveCreateImage(cmd.Context(), o.client(), image, pull, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				req.Image = &api.ImageRef{OCI: ref}
				req.Pull = effPull
			}
			if len(netAllow) > 0 {
				req.Network = &api.NetworkRequest{Enabled: true, Allowlist: netAllow}
			}
			for _, p := range publish {
				pm, err := parsePublish(p)
				if err != nil {
					return err
				}
				req.Publish = append(req.Publish, pm)
			}
			sb, err := o.client().CreateSandbox(cmd.Context(), req)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), sb)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), sb.ID)
			return nil
		},
	}
	cmd.Flags().IntVar(&vcpus, "vcpus", 0, "vCPUs (0 = daemon default)")
	cmd.Flags().IntVar(&memory, "memory", 0, "memory in MiB (0 = daemon default)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "max lifetime in seconds (0 = none)")
	cmd.Flags().StringVar(&profile, "profile", "", "rootfs profile (e.g. python-3.12)")
	cmd.Flags().StringVar(&image, "image", "", "boot from an image: a converted digest/ref, a local docker tag (auto-imported), or a registry ref (auto-pulled)")
	cmd.Flags().StringVar(&pull, "pull", "", "image pull policy: missing (default), always, or never")
	cmd.Flags().StringSliceVar(&netAllow, "net-allow", nil, "allowlisted hostname (repeatable); enables networking")
	cmd.Flags().StringArrayVarP(&publish, "publish", "p", nil, "publish a host port to a guest port: [HOST_IP:]HOST:GUEST[/tcp] (repeatable)")
	return cmd
}

// parsePublish parses a docker-style port spec:
//
//	HOST:GUEST              8080:80
//	HOST_IP:HOST:GUEST      127.0.0.1:8080:80
//	…with an optional /tcp suffix (tcp is the default).
func parsePublish(spec string) (api.PortMapping, error) {
	var pm api.PortMapping
	body, proto, hasProto := strings.Cut(spec, "/")
	pm.Protocol = "tcp"
	if hasProto {
		pm.Protocol = proto
	}
	parts := strings.Split(body, ":")
	var hostStr, guestStr string
	switch len(parts) {
	case 2:
		hostStr, guestStr = parts[0], parts[1]
	case 3:
		pm.HostIP, hostStr, guestStr = parts[0], parts[1], parts[2]
	default:
		return pm, fmt.Errorf("--publish %q: want [HOST_IP:]HOST:GUEST[/tcp]", spec)
	}
	hp, err := strconv.Atoi(hostStr)
	if err != nil {
		return pm, fmt.Errorf("--publish %q: bad host port %q", spec, hostStr)
	}
	gp, err := strconv.Atoi(guestStr)
	if err != nil {
		return pm, fmt.Errorf("--publish %q: bad guest port %q", spec, guestStr)
	}
	pm.HostPort, pm.GuestPort = hp, gp
	return pm, nil
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
			_, _ = fmt.Fprintln(tw, "ID\tPROFILE\tVCPUS\tMEM(MiB)\tNET\tAGE")
			for _, s := range sbs {
				prof := s.Profile
				if prof == "" {
					prof = "-"
				}
				net := "-"
				if s.Network != nil && s.Network.GuestIP != "" {
					net = s.Network.GuestIP
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", s.ID, prof, s.VCPUs, s.MemoryMiB, net, age(s.CreatedAt))
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
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
}

func newSandboxExecCmd(o *globalOpts) *cobra.Command {
	var (
		cwd         string
		timeout     int
		env         []string
		interactive bool
	)
	cmd := &cobra.Command{
		Use:   "exec <id> -- <command>...",
		Short: "Run a command in a sandbox and stream its output",
		Long: "Run a command in a sandbox and stream its stdout/stderr. The process " +
			"exit code becomes crucible's exit code. Use -- to separate the command " +
			"from crucible's own flags.\n\n" +
			"With -i/--interactive the command's stdin is connected to crucible's " +
			"stdin over a full-duplex stream, so you can drive a long-lived process " +
			"(e.g. a shell). There is no PTY: it is a functional, line-buffered " +
			"session, not a full terminal. See `crucible shell` for a shortcut.",
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
			res, err := runExec(cmd, o, args[0], req, interactive)
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
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "attach stdin for an interactive session (no PTY)")
	return cmd
}

// runExec dispatches to the one-shot or interactive client exec path,
// wiring the cobra command's stdio.
func runExec(cmd *cobra.Command, o *globalOpts, id string, req agentwire.ExecRequest, interactive bool) (agentwire.ExecResult, error) {
	if interactive {
		return o.client().ExecInteractive(cmd.Context(), id, req, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	return o.client().Exec(cmd.Context(), id, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// newShellCmd is a friendly alias for `sandbox exec -i <id> -- <shell>`:
// open a long-lived interactive shell into a running sandbox. State (cwd,
// env, shell variables) persists for the life of the session. No PTY.
func newShellCmd(o *globalOpts) *cobra.Command {
	var shellPath string
	cmd := &cobra.Command{
		Use:   "shell <id>",
		Short: "Open an interactive shell in a sandbox",
		Long: "Open a long-lived interactive shell into a running sandbox. Commands " +
			"share state (cwd, environment) across the session. There is no PTY, so " +
			"full-screen programs, colors, and Ctrl-C job control are not supported — " +
			"it is a functional shell for exploring untrusted code.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := agentwire.ExecRequest{Cmd: []string{shellPath}}
			res, err := o.client().ExecInteractive(cmd.Context(), args[0], req, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return exitCodeError{res.ExitCode}
			}
			return nil
		},
	}
	// Default to an absolute path: OCI-image sandboxes run the agent as PID 1,
	// which spawns via os.StartProcess (no PATH lookup), so a bare "sh" would
	// not resolve. /bin/sh exists in virtually every image and rootfs.
	cmd.Flags().StringVar(&shellPath, "shell", "/bin/sh", "shell to launch inside the sandbox")
	return cmd
}
