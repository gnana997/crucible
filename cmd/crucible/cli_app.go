package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// newAppCmd is the `crucible app` command group: durable apps the daemon
// keeps a healthy instance of and re-creates after a restart.
func newAppCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage durable apps (survive restart, self-heal)",
		Long:  "A durable app is a named workload the daemon keeps a healthy instance of, restarting it on failure and re-creating it from spec after a daemon restart or host reboot.",
	}
	cmd.AddCommand(
		newAppCreateCmd(o),
		newAppUpdateCmd(o),
		newAppListCmd(o),
		newAppGetCmd(o),
		newAppRmCmd(o),
		newAppLogsCmd(o),
		newAppExecCmd(o),
		newAppShellCmd(o),
	)
	return cmd
}

// appSpecOpts holds the flags shared by `app create` and `app update` and
// builds an AppSpec from them, so the two commands can never drift.
type appSpecOpts struct {
	image, pull, restart, health, healthCmd, disk string
	vcpus, memory                                 int
	netAllow, publish, env, netAllowCIDR          []string
	netFullEgress, publishAll                     bool
}

func (a *appSpecOpts) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&a.image, "image", "", "OCI image the app boots from (required)")
	f.StringVar(&a.pull, "pull", "", "image pull policy: missing|always|never")
	f.StringVar(&a.restart, "restart", wire.RestartAlways, "instance restart policy: always|on-failure|never")
	f.StringVar(&a.health, "health", "", "health check: http:PORT[:PATH] or tcp:PORT (e.g. http:80:/ )")
	f.StringVar(&a.healthCmd, "health-cmd", "", "exec health check: a shell command run in the guest, exit 0 = healthy (e.g. 'pg_isready -U postgres')")
	f.IntVar(&a.vcpus, "vcpus", 0, "vCPUs (0 = daemon default)")
	f.IntVar(&a.memory, "memory", 0, "memory in MiB (0 = daemon default)")
	f.StringVar(&a.disk, "disk", "", "writable rootfs size (e.g. 2G)")
	f.StringArrayVar(&a.netAllow, "net-allow", nil, "egress hostname allowlist entry (repeatable)")
	f.StringArrayVar(&a.netAllowCIDR, "net-allow-cidr", nil, "allow direct egress to a public IPv4 CIDR, e.g. 203.0.113.0/24 (repeatable)")
	f.BoolVar(&a.netFullEgress, "net-full-egress", false, "allow egress to any public host (metadata/link-local/RFC1918 still blocked)")
	f.StringArrayVarP(&a.publish, "publish", "p", nil, "publish a host port [HOST_IP:]HOST:GUEST[/tcp] (repeatable)")
	f.BoolVarP(&a.publishAll, "publish-all", "P", false, "publish every port the image EXPOSEs (guest N → host N)")
	f.StringArrayVarP(&a.env, "env", "e", nil, "environment variable KEY=VALUE for the app's entrypoint (repeatable)")
}

func (a *appSpecOpts) build(cmd *cobra.Command, o *globalOpts, name string) (api.AppSpec, error) {
	if a.image == "" {
		return api.AppSpec{}, fmt.Errorf("--image is required")
	}
	ref, effPull, err := resolveCreateImage(cmd.Context(), o.client(), a.image, a.pull, cmd.ErrOrStderr())
	if err != nil {
		return api.AppSpec{}, err
	}
	diskBytes, err := parseDiskSize(a.disk)
	if err != nil {
		return api.AppSpec{}, err
	}
	envMap, err := api.ParseEnv(a.env)
	if err != nil {
		return api.AppSpec{}, err
	}
	spec := api.AppSpec{
		Name:       name,
		Image:      &api.ImageRef{OCI: ref},
		Pull:       effPull,
		VCPUs:      a.vcpus,
		MemoryMiB:  a.memory,
		DiskBytes:  diskBytes,
		Env:        envMap,
		PublishAll: a.publishAll,
		Restart:    wire.RestartPolicy{Policy: a.restart},
	}
	for _, p := range a.publish {
		pm, perr := parsePublish(p)
		if perr != nil {
			return api.AppSpec{}, perr
		}
		spec.Publish = append(spec.Publish, pm)
	}
	spec.Network = buildNetworkRequest(a.netAllow, a.netAllowCIDR, a.netFullEgress)
	if a.health != "" {
		hc, herr := parseHealth(a.health)
		if herr != nil {
			return api.AppSpec{}, herr
		}
		spec.Health = hc
	}
	if a.healthCmd != "" {
		if spec.Health != nil {
			return api.AppSpec{}, fmt.Errorf("--health and --health-cmd are mutually exclusive")
		}
		// Shell form (docker HEALTHCHECK CMD-SHELL): exit 0 = healthy.
		spec.Health = &api.HealthCheck{Type: "exec", Cmd: []string{"/bin/sh", "-c", a.healthCmd}}
	}
	return spec, nil
}

func newAppCreateCmd(o *globalOpts) *cobra.Command {
	var opts appSpecOpts
	var stopped bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a durable app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := opts.build(cmd, o, args[0])
			if err != nil {
				return err
			}
			resp, err := o.client().CreateApp(cmd.Context(), api.CreateAppRequest{
				AppSpec:      spec,
				DesiredState: desiredState(stopped),
			})
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), resp)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.Name)
			return nil
		},
	}
	opts.register(cmd)
	cmd.Flags().BoolVar(&stopped, "stopped", false, "create the app without starting an instance")
	return cmd
}

func newAppUpdateCmd(o *globalOpts) *cobra.Command {
	var opts appSpecOpts
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a durable app's spec and redeploy it",
		Long:  "Replace the app's spec (same flags as create) and redeploy its instance — the old instance is destroyed and a fresh one is booted from the new spec. The app's name is immutable; desired running/stopped is retained.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := opts.build(cmd, o, args[0])
			if err != nil {
				return err
			}
			resp, err := o.client().UpdateApp(cmd.Context(), args[0], spec)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), resp)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.Name)
			return nil
		},
	}
	opts.register(cmd)
	return cmd
}

func newAppListCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List apps",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			page, err := o.client().ListApps(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), page.Items)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "NAME\tDESIRED\tPHASE\tHEALTH\tRESTARTS\tINSTANCE")
			for _, a := range page.Items {
				phase, health, restarts, inst := "-", "-", 0, "-"
				if a.Status != nil {
					phase, health, restarts = orDash(a.Status.Phase), orDash(a.Status.Health), a.Status.Restarts
					inst = orDash(a.Status.InstanceID)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", a.Name, a.DesiredState, phase, health, restarts, inst)
			}
			return tw.Flush()
		},
	}
}

func newAppGetCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show an app's desired state and observed status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := o.client().GetApp(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), resp)
		},
	}
}

func newAppRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Delete an app and tear down its instance",
		Aliases: []string{"delete"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.client().DeleteApp(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
}

func newAppLogsCmd(o *globalOpts) *cobra.Command {
	var follow bool
	var source string
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Tail the app instance's durable logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := o.appInstanceID(cmd, args[0])
			if err != nil {
				return err
			}
			return runLogs(cmd, o, id, source, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines")
	cmd.Flags().StringVar(&source, "source", "all", "log source: service|exec|all")
	return cmd
}

func newAppExecCmd(o *globalOpts) *cobra.Command {
	var interactive bool
	cmd := &cobra.Command{
		Use:   "exec <name> -- <cmd> [args...]",
		Short: "Run a command in the app's current instance",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := o.appInstanceID(cmd, args[0])
			if err != nil {
				return err
			}
			res, eerr := runExec(cmd, o, id, wire.ExecRequest{Cmd: args[1:]}, interactive)
			if eerr != nil {
				return eerr
			}
			return exitFromResult(res)
		},
	}
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "attach stdin (full-duplex)")
	return cmd
}

func newAppShellCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in the app's current instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := o.appInstanceID(cmd, args[0])
			if err != nil {
				return err
			}
			res, err := runExec(cmd, o, id, wire.ExecRequest{Cmd: []string{"/bin/sh"}}, true)
			if err != nil {
				return err
			}
			return exitFromResult(res)
		},
	}
}

// appInstanceID resolves an app name to its current instance (sandbox) id.
func (o *globalOpts) appInstanceID(cmd *cobra.Command, name string) (string, error) {
	resp, err := o.client().GetApp(cmd.Context(), name)
	if err != nil {
		return "", err
	}
	if resp.Status == nil || resp.Status.InstanceID == "" {
		phase := "pending"
		if resp.Status != nil {
			phase = resp.Status.Phase
		}
		return "", fmt.Errorf("app %q has no running instance (phase %q)", name, phase)
	}
	return resp.Status.InstanceID, nil
}

func desiredState(stopped bool) string {
	if stopped {
		return "stopped"
	}
	return "running"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseHealth parses "http:PORT[:PATH]" or "tcp:PORT" into a HealthCheck.
func parseHealth(spec string) (*api.HealthCheck, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("health %q: want http:PORT[:PATH] or tcp:PORT", spec)
	}
	typ := parts[0]
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("health %q: bad port %q", spec, parts[1])
	}
	hc := &api.HealthCheck{Type: typ, Port: port}
	switch typ {
	case "http":
		hc.Path = "/"
		if len(parts) == 3 && parts[2] != "" {
			hc.Path = parts[2]
		}
	case "tcp":
		if len(parts) == 3 {
			return nil, fmt.Errorf("health %q: tcp takes no path", spec)
		}
	default:
		return nil, fmt.Errorf("health %q: type must be http or tcp", spec)
	}
	return hc, nil
}

// exitFromResult mirrors the sandbox exec exit-code propagation.
func exitFromResult(res wire.ExecResult) error {
	if res.ExitCode != 0 {
		return exitCodeError{res.ExitCode}
	}
	return nil
}
