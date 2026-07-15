package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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
		newAppSleepCmd(o),
		newAppWakeCmd(o),
		newAppLogsCmd(o),
		newAppExecCmd(o),
		newAppShellCmd(o),
		newAppCaptureCmd(o),
		newAppDomainCmd(o),
		newAppUsageCmd(o),
	)
	return cmd
}

// newAppUsageCmd is `crucible app usage [<name>]` — the persistent usage
// metrics (durable per-app counters that survive a daemon restart). With no
// name it lists every app (including retained records for deleted apps).
func newAppUsageCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "usage [name]",
		Short: "Show durable per-app usage counters (compute/memory/requests/storage)",
		Long: "Persistent usage metrics: cumulative, monotonic per-app counters the daemon\n" +
			"keeps across restarts. Values are cumulative — diff two readings to get usage\n" +
			"over a window. Compute/memory accrue only while an app is awake; storage\n" +
			"accrues while its volume exists. With no name, lists every app.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var rows []api.AppUsage
			if len(args) == 1 {
				u, err := o.client().AppUsage(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				rows = []api.AppUsage{u}
			} else {
				resp, err := o.client().Usage(cmd.Context())
				if err != nil {
					return err
				}
				rows = resp.Usage
			}
			if o.isJSON() {
				if len(args) == 1 {
					return printJSON(cmd.OutOrStdout(), rows[0])
				}
				return printJSON(cmd.OutOrStdout(), rows)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "APP\tSTATE\tCOMPUTE(vCPU·h)\tMEM(MiB·h)\tSTORAGE(GiB·h)\tREQUESTS")
			for _, u := range rows {
				state := "live"
				if u.FinalizedAt != nil {
					state = "deleted"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%.4f\t%.4f\t%.4f\t%d\n",
					u.AppName, state,
					u.ComputeVCPUSeconds/3600, u.MemoryMiBSeconds/3600, u.StorageGiBSeconds/3600,
					u.Requests)
			}
			return tw.Flush()
		},
	}
}

// newAppDomainCmd is `crucible app domain add|rm|ls` — manage the custom domains
// the ingress proxy routes (and, in terminate mode, certifies) for an app.
func newAppDomainCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Manage an app's custom domains (attach/detach/list)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "add <app> <domain>",
			Short: "Attach a custom domain to an app (globally unique)",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				resp, err := o.client().AddDomain(cmd.Context(), args[0], args[1])
				if err != nil {
					return err
				}
				if o.isJSON() {
					return printJSON(cmd.OutOrStdout(), resp)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), args[1])
				return nil
			},
		},
		&cobra.Command{
			Use:     "rm <app> <domain>",
			Short:   "Detach a custom domain from an app",
			Aliases: []string{"remove"},
			Args:    cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return o.client().RemoveDomain(cmd.Context(), args[0], args[1])
			},
		},
		&cobra.Command{
			Use:     "ls <app>",
			Short:   "List an app's domains with TLS/certificate status",
			Aliases: []string{"list"},
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				details, err := o.client().ListDomainsDetail(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if o.isJSON() {
					return printJSON(cmd.OutOrStdout(), details)
				}
				tw := newTable(cmd.OutOrStdout())
				_, _ = fmt.Fprintln(tw, "DOMAIN\tKIND\tTLS\tCERT\tEXPIRES")
				for _, d := range details {
					kind := "custom"
					if d.Generated {
						kind = "generated"
					}
					expires := "-"
					if d.Cert.NotAfter != nil {
						expires = d.Cert.NotAfter.Format("2006-01-02")
					}
					cert := d.Cert.State
					if d.Cert.State == "failed" && d.Cert.LastError != "" {
						cert = "failed: " + d.Cert.LastError
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Domain, kind, d.TLSMode, cert, expires)
				}
				return tw.Flush()
			},
		},
	)
	return cmd
}

func newAppSleepCmd(o *globalOpts) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "sleep <name> | --all",
		Short: "Snapshot the app and free its RAM (scale-to-zero); it wakes on demand",
		Long: "Snapshot an app and stop its VM: RAM drops to ~zero, identity and data are\n" +
			"kept, and the next request (or `app wake`) restores it in place.\n\n" +
			"--all sleeps every running app, sequentially — the drain step of the\n" +
			"upgrade-without-drop runbook (docs/upgrades.md): drain, restart the daemon,\n" +
			"and slept apps are re-adopted and wake on demand.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all {
				if len(args) != 1 {
					return errors.New("app sleep: give an app name, or --all")
				}
				resp, err := o.client().SleepApp(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), resp)
			}
			if len(args) != 0 {
				return errors.New("app sleep: --all takes no app name")
			}
			return sleepAllApps(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "sleep every running app (drain, e.g. before a daemon upgrade)")
	return cmd
}

// sleepAllApps sleeps every running app sequentially and reports per-app
// results. Already-asleep apps count as fine; apps in any other phase
// (pending, crashlooping, stopped) are skipped — sleep requires running.
// Returns an error (nonzero exit) if any sleep failed.
func sleepAllApps(cmd *cobra.Command, o *globalOpts) error {
	page, err := o.client().ListApps(cmd.Context())
	if err != nil {
		return err
	}
	type result struct {
		Name   string `json:"name"`
		Result string `json:"result"` // slept | already-asleep | skipped (<phase>) | error
		Error  string `json:"error,omitempty"`
	}
	var results []result
	failed := 0
	for _, a := range page.Items {
		phase := ""
		if a.Status != nil {
			phase = a.Status.Phase
		}
		switch phase {
		case "running", "unhealthy":
			if _, err := o.client().SleepApp(cmd.Context(), a.Name); err != nil {
				failed++
				results = append(results, result{Name: a.Name, Result: "error", Error: err.Error()})
			} else {
				results = append(results, result{Name: a.Name, Result: "slept"})
			}
		case "asleep":
			results = append(results, result{Name: a.Name, Result: "already-asleep"})
		default:
			results = append(results, result{Name: a.Name, Result: "skipped (" + phase + ")"})
		}
	}
	if o.isJSON() {
		if err := printJSON(cmd.OutOrStdout(), results); err != nil {
			return err
		}
	} else {
		tw := newTable(cmd.OutOrStdout())
		_, _ = fmt.Fprintln(tw, "APP\tRESULT")
		for _, r := range results {
			line := r.Result
			if r.Error != "" {
				line += ": " + r.Error
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", r.Name, line)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("app sleep --all: %d of %d apps failed to sleep", failed, len(results))
	}
	return nil
}

func newAppWakeCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "wake <name>",
		Short: "Restore a slept app in place (same IP), correcting its clock",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := o.client().WakeApp(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), resp)
		},
	}
}

// appSpecOpts holds the flags shared by `app create` and `app update` and
// builds an AppSpec from them, so the two commands can never drift.
type appSpecOpts struct {
	image, pull, restart, health, healthCmd, disk string
	idleTimeout, connIdleTimeout, tlsMode         string
	vcpus, memory, port, minScale                 int
	maxScale, targetConcurrency                   int
	netAllow, publish, env, netAllowCIDR, canCall []string
	volumes                                       []string
	netFullEgress, publishAll, keepConnections    bool
	noHTTPSRedirect                               bool
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
	f.IntVar(&a.port, "port", 0, "guest port the ingress proxy forwards to (0 = default from a single published port)")
	f.StringVar(&a.disk, "disk", "", "writable rootfs size (e.g. 2G)")
	f.StringArrayVar(&a.netAllow, "net-allow", nil, "egress hostname allowlist entry (repeatable)")
	f.StringArrayVar(&a.netAllowCIDR, "net-allow-cidr", nil, "allow direct egress to a public IPv4 CIDR, e.g. 203.0.113.0/24 (repeatable)")
	f.BoolVar(&a.netFullEgress, "net-full-egress", false, "allow egress to any public host (metadata/link-local/RFC1918 still blocked)")
	f.StringArrayVarP(&a.publish, "publish", "p", nil, "publish a host port [HOST_IP:]HOST:GUEST[/tcp] (repeatable)")
	f.StringArrayVar(&a.volumes, "volume", nil, "attach a persistent volume NAME:/path (repeatable); the app becomes single-writer (destroy-then-boot redeploy, stop/start sleep) and its data survives redeploys/sleep/restarts")
	f.BoolVarP(&a.publishAll, "publish-all", "P", false, "publish every port the image EXPOSEs (guest N → host N)")
	f.StringArrayVarP(&a.env, "env", "e", nil, "environment variable KEY=VALUE for the app's entrypoint (repeatable)")
	f.StringVar(&a.idleTimeout, "idle-timeout", "", "auto-sleep (scale-to-zero) after this idle duration, e.g. 30s (wakes on the next request via the ingress proxy, or on the next TCP connection to a published port)")
	f.IntVar(&a.minScale, "min-scale", 0, "minimum warm instances: 0 = may sleep when idle, 1 = keep one running")
	f.IntVar(&a.maxScale, "max-scale", 0, "maximum instances for horizontal autoscaling; >min-scale enables it (0 = fixed at min-scale)")
	f.IntVar(&a.targetConcurrency, "target-concurrency", 0, "autoscaler target: in-flight requests per instance (0 = default)")
	f.StringVar(&a.connIdleTimeout, "connection-idle-timeout", "", "for a scale-to-zero published (TCP) app: close a connection idle this long so pooled clients let it sleep (e.g. 30s; default = --idle-timeout)")
	f.BoolVar(&a.keepConnections, "keep-connections", false, "for a scale-to-zero published (TCP) app: never reap idle connections — sleep only when the last client disconnects (pub/sub, LISTEN, streaming)")
	f.StringArrayVar(&a.canCall, "can-call", nil, "app this app may reach at <app>.internal via the ingress proxy — app→app networking, default-deny (repeatable; needs the daemon's --internal-networking)")
	f.StringVar(&a.tlsMode, "tls-mode", "", "how the ingress proxy handles this app's HTTPS on :443: terminate (default, the proxy manages the cert) or passthrough (the guest owns its cert)")
	f.BoolVar(&a.noHTTPSRedirect, "no-https-redirect", false, "serve plain HTTP on :80 for this app instead of 301-redirecting to HTTPS (only meaningful with TLS termination)")
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
		Port:       a.port,
		PublishAll: a.publishAll,
		Restart:    wire.RestartPolicy{Policy: a.restart},
		CanCall:    a.canCall,
		TLSMode:    a.tlsMode,
	}
	if a.noHTTPSRedirect {
		off := false
		spec.HTTPRedirect = &off
	}
	for _, p := range a.publish {
		pm, perr := parsePublish(p)
		if perr != nil {
			return api.AppSpec{}, perr
		}
		spec.Publish = append(spec.Publish, pm)
	}
	for _, vspec := range a.volumes {
		vm, verr := parseVolume(vspec)
		if verr != nil {
			return api.AppSpec{}, verr
		}
		spec.Volumes = append(spec.Volumes, vm)
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
	if cmd.Flags().Changed("idle-timeout") || cmd.Flags().Changed("min-scale") ||
		cmd.Flags().Changed("max-scale") || cmd.Flags().Changed("target-concurrency") ||
		cmd.Flags().Changed("connection-idle-timeout") || cmd.Flags().Changed("keep-connections") {
		idleSec, err := parseIdleTimeout(a.idleTimeout)
		if err != nil {
			return api.AppSpec{}, err
		}
		connIdleSec, err := parseIdleTimeout(a.connIdleTimeout)
		if err != nil {
			return api.AppSpec{}, fmt.Errorf("--connection-idle-timeout: %w", err)
		}
		spec.Sleep = &api.SleepPolicy{
			IdleTimeoutSec:     idleSec,
			MinScale:           a.minScale,
			MaxScale:           a.maxScale,
			TargetConcurrency:  a.targetConcurrency,
			ConnIdleTimeoutSec: connIdleSec,
			KeepConnections:    a.keepConnections,
		}
	}
	return spec, nil
}

// parseIdleTimeout turns a duration string ("30s", "5m") into whole seconds; ""
// means zero (no idle timeout).
func parseIdleTimeout(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--idle-timeout: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("--idle-timeout must be non-negative")
	}
	return int(d.Seconds()), nil
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
			_, _ = fmt.Fprintln(tw, "NAME\tDESIRED\tPHASE\tHEALTH\tREPLICAS\tRESTARTS\tINSTANCE")
			for _, a := range page.Items {
				phase, health, restarts, inst, replicas := "-", "-", 0, "-", "-"
				if a.Status != nil {
					phase, health, restarts = orDash(a.Status.Phase), orDash(a.Status.Health), a.Status.Restarts
					inst = orDash(a.Status.InstanceID)
					replicas = fmt.Sprintf("%d/%d", a.Status.ReadyReplicas, a.Status.Replicas)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", a.Name, a.DesiredState, phase, health, replicas, restarts, inst)
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
			return runAppLogs(cmd, o, args[0], source, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines (reattaches across a redeploy)")
	cmd.Flags().StringVar(&source, "source", "all", "log source: service|exec|all")
	return cmd
}

func newAppExecCmd(o *globalOpts) *cobra.Command {
	var (
		cwd         string
		timeout     int
		env         []string
		interactive bool
	)
	cmd := &cobra.Command{
		Use:   "exec <name> -- <cmd> [args...]",
		Short: "Run a command in the app's current instance",
		Long: "Run a command in the app's current instance and stream its output. The " +
			"app name is resolved to its current instance by the daemon on each call, " +
			"so this keeps working across a self-heal or rolling update. Use -- to " +
			"separate the command from crucible's own flags.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := wire.ExecRequest{Cmd: args[1:], Cwd: cwd, TimeoutSec: timeout}
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
			res, eerr := runAppExec(cmd, o, args[0], req, interactive)
			if eerr != nil {
				return eerr
			}
			return exitFromResult(res)
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory inside the guest")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "command deadline in seconds (0 = none)")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "environment KEY=VALUE (repeatable)")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "attach stdin (full-duplex)")
	return cmd
}

func newAppShellCmd(o *globalOpts) *cobra.Command {
	var shellPath string
	cmd := &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in the app's current instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runAppExec(cmd, o, args[0], wire.ExecRequest{Cmd: []string{shellPath}}, true)
			if err != nil {
				return err
			}
			return exitFromResult(res)
		},
	}
	cmd.Flags().StringVar(&shellPath, "shell", "/bin/sh", "shell to launch inside the app instance")
	return cmd
}

// runAppExec runs an exec against an app BY NAME via the app-scoped routes, so
// the daemon resolves the current instance server-side per request — correct
// across a self-heal or rolling update without the client capturing an id.
func runAppExec(cmd *cobra.Command, o *globalOpts, name string, req wire.ExecRequest, interactive bool) (wire.ExecResult, error) {
	if interactive {
		return o.client().AppExecInteractive(cmd.Context(), name, req, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	return o.client().AppExec(cmd.Context(), name, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runAppLogs prints an app's current-instance logs and, with --follow, tails
// them — re-resolving the instance each poll so a self-heal or rolling update
// reattaches the follow to the new instance instead of dying on a stale id.
func runAppLogs(cmd *cobra.Command, o *globalOpts, name, source string, follow bool) error {
	switch source {
	case "", "all", "service", "exec":
	default:
		return fmt.Errorf("--source must be service, exec, or all")
	}
	out := cmd.OutOrStdout()
	inst, err := o.appInstanceID(cmd.Context(), name)
	if err != nil {
		return err
	}
	resp, err := o.client().Logs(cmd.Context(), inst, -1, source)
	if err != nil {
		return err
	}
	if o.isJSON() {
		return printJSON(out, resp)
	}
	for _, rec := range resp.Records {
		renderLogRecord(out, rec)
	}
	if !follow {
		return nil
	}
	return followAppLogs(cmd.Context(), o, name, inst, source, resp.NextOffset, out)
}

// followAppLogs tails an app's logs, re-resolving its current instance every
// tick. When the instance changes (self-heal / rolling update) it prints a
// reattach marker and resumes tailing the new instance from its recent log.
func followAppLogs(ctx context.Context, o *globalOpts, name, inst, source string, since int64, out io.Writer) error {
	cl := o.client()
	ticker := time.NewTicker(logsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		cur, err := o.appInstanceID(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue // no ready instance right now (e.g. mid-redeploy) — keep waiting
		}
		if cur != inst {
			_, _ = fmt.Fprintf(out, "== reattached to %s ==\n", cur)
			inst, since = cur, -1
		}
		resp, err := cl.Logs(ctx, inst, since, source)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue // the instance may have just been replaced; re-resolve next tick
		}
		for _, rec := range resp.Records {
			renderLogRecord(out, rec)
		}
		since = resp.NextOffset
	}
}

// appInstanceID resolves an app name to its current instance (sandbox) id.
func (o *globalOpts) appInstanceID(ctx context.Context, name string) (string, error) {
	resp, err := o.client().GetApp(ctx, name)
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
