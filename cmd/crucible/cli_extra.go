package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/version"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// newTable returns a tabwriter tuned for the CLI's aligned list output.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

// hintCmd writes one aligned "    label   command" suggestion line — the human
// "here's how to manage this" context printed (on stderr) under a machine id.
// Keeps multi-command hints readable instead of cramming them onto one line.
func hintCmd(w io.Writer, label, command string) {
	_, _ = fmt.Fprintf(w, "    %-8s%s\n", label, command)
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
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "crucible %s\n", version.String())
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
	var publishSpecs []string
	cmd := &cobra.Command{
		Use:   "fork <snapshot-id>",
		Short: "Fork one or more sandboxes from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var publish []api.PortMapping
			for _, spec := range publishSpecs {
				pm, err := api.ParsePublish(spec)
				if err != nil {
					return err
				}
				publish = append(publish, pm)
			}
			forks, err := o.client().Fork(cmd.Context(), args[0], count, publish...)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), forks)
			}
			for _, f := range forks {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), f.ID)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&count, "count", 1, "number of forks to create")
	cmd.Flags().StringArrayVarP(&publishSpecs, "publish", "p", nil, "publish a fork port [HOST_IP:]HOST:GUEST[/tcp] (requires --count 1)")
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
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no profiles configured (daemon started without --rootfs-dir)")
				return nil
			}
			for _, p := range profs {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), p)
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
		netAllowCIDR           []string
		netFullEgress          bool
		publish                []string
		publishAll             bool
		pull                   string
		disk                   string
		keep, rm               bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] <image>   |   run [flags] -- <command>...",
		Short: "Run an image as a sandbox (docker-parity), or a command in a throwaway sandbox",
		Long: "Two shapes:\n\n" +
			"  crucible run <image>            boot an OCI image as a sandbox (its\n" +
			"                                  entrypoint runs as the service). Publish\n" +
			"                                  ports with -p, allow egress with\n" +
			"                                  --net-allow. Long-lived by default — it is\n" +
			"                                  NOT auto-killed; stop it with `crucible\n" +
			"                                  stop <id>` or remove it with `crucible rm\n" +
			"                                  <id>`. --rm tails logs in the foreground\n" +
			"                                  and removes the sandbox when you detach.\n\n" +
			"  crucible run -- <command>...    create a throwaway sandbox (a --profile,\n" +
			"                                  or the daemon default), run one command,\n" +
			"                                  stream its output, then delete it (unless\n" +
			"                                  --keep). The command's exit code becomes\n" +
			"                                  crucible's.\n\n" +
			"The `--` separator selects command mode; a bare positional selects image mode.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A `--` in the args (ArgsLenAtDash >= 0) selects command mode;
			// its absence selects image mode. This keeps the pre-existing
			// `run -- <cmd>` behavior while making `run <image>` the headline.
			if cmd.ArgsLenAtDash() == -1 {
				if len(args) != 1 {
					return fmt.Errorf("run <image> takes exactly one image; use `run -- <command>` to run a command")
				}
				diskBytes, err := parseDiskSize(disk)
				if err != nil {
					return err
				}
				return runImage(cmd, o, args[0], runImageOpts{
					vcpus: vcpus, memory: memory, timeout: timeout,
					netAllow: netAllow, netAllowCIDR: netAllowCIDR, netFullEgress: netFullEgress,
					publish: publish, publishAll: publishAll,
					pull: pull, rm: rm, diskBytes: diskBytes,
				})
			}
			return runCommand(cmd, o, args, runCommandOpts{
				vcpus: vcpus, memory: memory, timeout: timeout,
				profile: profile, netAllow: netAllow, netAllowCIDR: netAllowCIDR,
				netFullEgress: netFullEgress, keep: keep,
			})
		},
	}
	cmd.Flags().IntVar(&vcpus, "vcpus", 0, "vCPUs (0 = daemon default)")
	cmd.Flags().IntVar(&memory, "memory", 0, "memory in MiB (0 = daemon default)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "timeout in seconds (0 = long-lived / no deadline)")
	cmd.Flags().StringSliceVar(&netAllow, "net-allow", nil, "allowlisted hostname (repeatable); enables networking")
	cmd.Flags().StringArrayVar(&netAllowCIDR, "net-allow-cidr", nil, "allow direct egress to a public IPv4 CIDR, e.g. 203.0.113.0/24 (repeatable)")
	cmd.Flags().BoolVar(&netFullEgress, "net-full-egress", false, "allow egress to any public host (metadata/link-local/RFC1918 still blocked)")
	// image mode
	cmd.Flags().StringArrayVarP(&publish, "publish", "p", nil, "publish a port [HOST_IP:]HOST:GUEST[/tcp] (repeatable; image mode)")
	cmd.Flags().BoolVarP(&publishAll, "publish-all", "P", false, "publish every port the image EXPOSEs (guest N → host N; image mode)")
	cmd.Flags().StringVar(&pull, "pull", "", "image pull policy: missing|always|never (image mode)")
	cmd.Flags().StringVar(&disk, "disk", "", "grow the writable rootfs to this size, e.g. 2G (image mode)")
	cmd.Flags().BoolVar(&rm, "rm", false, "tail logs in the foreground and remove the sandbox on detach (image mode)")
	// command mode
	cmd.Flags().StringVar(&profile, "profile", "", "rootfs profile, e.g. python-3.12 (command mode)")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox instead of deleting it after the command (command mode)")
	return cmd
}

type runCommandOpts struct {
	vcpus, memory, timeout int
	profile                string
	netAllow               []string
	netAllowCIDR           []string
	netFullEgress          bool
	keep                   bool
}

// runCommand is the throwaway-command path: create → exec one command →
// delete (unless --keep). The command's exit code becomes crucible's.
func runCommand(cmd *cobra.Command, o *globalOpts, args []string, opts runCommandOpts) error {
	cl := o.client()
	req := api.CreateSandboxRequest{VCPUs: opts.vcpus, MemoryMiB: opts.memory, TimeoutSec: opts.timeout, Profile: opts.profile}
	req.Network = buildNetworkRequest(opts.netAllow, opts.netAllowCIDR, opts.netFullEgress)
	sb, err := cl.CreateSandbox(cmd.Context(), req)
	if err != nil {
		return err
	}
	if !opts.keep {
		// Background context so cleanup runs even if cmd.Context() was
		// cancelled (e.g. Ctrl-C during exec).
		defer func() { _ = cl.DeleteSandbox(context.Background(), sb.ID) }()
	}
	res, err := cl.Exec(cmd.Context(), sb.ID, wire.ExecRequest{Cmd: args, TimeoutSec: opts.timeout}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exitCodeError{res.ExitCode}
	}
	return nil
}

type runImageOpts struct {
	vcpus, memory, timeout int
	netAllow, publish      []string
	netAllowCIDR           []string
	netFullEgress          bool
	publishAll             bool
	pull                   string
	diskBytes              int64
	rm                     bool
}

// runImage is the docker-parity path: acquire the image (local Docker or the
// daemon store/registry, like `sandbox create --image`), create a sandbox
// that boots its entrypoint, publish ports, and report how to reach and stop
// it. With --rm it tails logs in the foreground and removes the sandbox when
// the user detaches (Ctrl-C).
func runImage(cmd *cobra.Command, o *globalOpts, image string, opts runImageOpts) error {
	cl := o.client()
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	ref, effPull, err := resolveCreateImage(cmd.Context(), cl, image, opts.pull, errOut)
	if err != nil {
		return err
	}
	req := api.CreateSandboxRequest{
		VCPUs: opts.vcpus, MemoryMiB: opts.memory, TimeoutSec: opts.timeout,
		Image: &api.ImageRef{OCI: ref}, Pull: effPull, DiskBytes: opts.diskBytes,
	}
	req.Network = buildNetworkRequest(opts.netAllow, opts.netAllowCIDR, opts.netFullEgress)
	for _, p := range opts.publish {
		pm, err := parsePublish(p)
		if err != nil {
			return err
		}
		req.Publish = append(req.Publish, pm)
	}
	req.PublishAll = opts.publishAll

	sb, err := cl.CreateSandbox(cmd.Context(), req)
	if err != nil {
		return err
	}

	// The id goes to stdout (scriptable); everything else is human context on
	// stderr so `SBX=$(crucible run img)` captures just the id.
	_, _ = fmt.Fprintln(out, sb.ID)
	for _, pm := range sb.Published {
		host := pm.HostIP
		if host == "" {
			host = "0.0.0.0"
		}
		_, _ = fmt.Fprintf(errOut, "  published %s:%d → guest :%d/%s\n", host, pm.HostPort, pm.GuestPort, protoOrTCP(pm.Protocol))
	}

	if opts.rm {
		// Foreground: tail logs until Ctrl-C, then remove. Background context
		// on delete so teardown runs even though cmd.Context() is cancelled.
		defer func() { _ = cl.DeleteSandbox(context.Background(), sb.ID) }()
		_, _ = fmt.Fprintf(errOut, "%s running — Ctrl-C to stop tailing and remove it\n", sb.ID)
		return tailUntilCancel(cmd, o, sb.ID)
	}

	_, _ = fmt.Fprintln(errOut, "  running (long-lived):")
	hintCmd(errOut, "logs", "crucible logs -f "+sb.ID)
	hintCmd(errOut, "stop", "crucible stop "+sb.ID)
	hintCmd(errOut, "remove", "crucible rm "+sb.ID)
	return nil
}

// tailUntilCancel streams a sandbox's logs to stdout until the context is
// cancelled. If the daemon has durable logs disabled it degrades to simply
// waiting for Ctrl-C (so --rm still removes the sandbox on exit).
func tailUntilCancel(cmd *cobra.Command, o *globalOpts, id string) error {
	out := cmd.OutOrStdout()
	resp, err := o.client().Logs(cmd.Context(), id, -1, "all")
	if err != nil {
		if cmd.Context().Err() != nil {
			return nil
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "logs unavailable (%v); waiting — Ctrl-C to remove %s\n", err, id)
		<-cmd.Context().Done()
		return nil
	}
	for _, rec := range resp.Records {
		renderLogRecord(out, rec)
	}
	return followLogs(cmd.Context(), o, id, "all", resp.NextOffset, out)
}

// protoOrTCP defaults an empty protocol to "tcp" for display.
func protoOrTCP(p string) string {
	if p == "" {
		return "tcp"
	}
	return p
}

// newStopCmd is the docker-parity graceful stop: send the image's StopSignal
// to the sandbox's entrypoint and wait the grace period before SIGKILL. The
// sandbox stays (use `crucible rm` to remove it).
func newStopCmd(o *globalOpts) *cobra.Command {
	var grace int
	cmd := &cobra.Command{
		Use:   "stop <id>...",
		Short: "Gracefully stop a sandbox's service (StopSignal + grace, then SIGKILL)",
		Long: "Stop the entrypoint running in one or more sandboxes: send the image's " +
			"stop signal, wait the grace period, then SIGKILL. The sandbox itself " +
			"remains — use `crucible rm` to remove it.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, id := range args {
				if _, err := cl.StopService(cmd.Context(), id, grace); err != nil {
					return fmt.Errorf("stop %s: %w", id, err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&grace, "grace", 0, "override the image's stop grace (seconds)")
	return cmd
}

// newRmCmd is a top-level alias for `sandbox rm`: the hard kill / remove.
func newRmCmd(o *globalOpts) *cobra.Command {
	c := newSandboxRmCmd(o)
	c.Short = "Remove (hard-kill) one or more sandboxes"
	return c
}
