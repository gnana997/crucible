package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/wire"
)

func newServiceCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage a sandbox's supervised service (experimental)",
		Long: "Configure and control the long-lived entrypoint the guest agent " +
			"supervises inside a sandbox: set the command, start/stop/restart it, " +
			"and read its status and captured logs.",
		Aliases: []string{"svc"},
	}
	cmd.AddCommand(
		newServiceSetCmd(o),
		newServiceStartCmd(o),
		newServiceStopCmd(o),
		newServiceRestartCmd(o),
		newServiceStatusCmd(o),
		newServiceLogsCmd(o),
	)
	return cmd
}

// parseRestart turns "never", "always", "on-failure" or "on-failure:N"
// into a RestartPolicy.
func parseRestart(s string) (wire.RestartPolicy, error) {
	if s == "" {
		return wire.RestartPolicy{}, nil
	}
	policy, retries, ok := strings.Cut(s, ":")
	rp := wire.RestartPolicy{Policy: policy}
	if ok {
		if policy != wire.RestartOnFailure {
			return rp, fmt.Errorf("--restart %q: max retries are only valid with on-failure", s)
		}
		if _, err := fmt.Sscanf(retries, "%d", &rp.MaxRetries); err != nil || rp.MaxRetries < 1 {
			return rp, fmt.Errorf("--restart %q: want on-failure:<positive retries>", s)
		}
	}
	return rp, nil
}

func newServiceSetCmd(o *globalOpts) *cobra.Command {
	var (
		env        []string
		cwd        string
		stopSignal string
		stopGrace  int
		restart    string
		start      bool
	)
	cmd := &cobra.Command{
		Use:   "set <id> -- <command>...",
		Short: "Configure (or replace) the sandbox's service",
		Long: "Install the supervised service spec. A running service is stopped " +
			"and relaunched under the new spec. Use --start to launch it right away.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := wire.ServiceSpec{
				Cmd:          args[1:],
				Cwd:          cwd,
				StopSignal:   stopSignal,
				StopGraceSec: stopGrace,
			}
			if len(env) > 0 {
				spec.Env = make(map[string]string, len(env))
				for _, kv := range env {
					k, v, ok := strings.Cut(kv, "=")
					if !ok {
						return fmt.Errorf("--env %q must be KEY=VALUE", kv)
					}
					spec.Env[k] = v
				}
			}
			rp, err := parseRestart(restart)
			if err != nil {
				return err
			}
			spec.Restart = rp

			cl := o.client()
			status, err := cl.ConfigureService(cmd.Context(), args[0], spec)
			if err != nil {
				return err
			}
			if start {
				status, err = cl.StartService(cmd.Context(), args[0])
				if err != nil {
					return err
				}
			}
			return printServiceStatus(cmd, o, status)
		},
	}
	cmd.Flags().StringArrayVar(&env, "env", nil, "environment KEY=VALUE (repeatable)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory inside the guest")
	cmd.Flags().StringVar(&stopSignal, "stop-signal", "", "signal sent on stop (default SIGTERM)")
	cmd.Flags().IntVar(&stopGrace, "stop-grace", 0, "seconds between stop signal and SIGKILL (default 10)")
	cmd.Flags().StringVar(&restart, "restart", "", "restart policy: never, always, on-failure[:max]")
	cmd.Flags().BoolVar(&start, "start", false, "start the service after configuring")
	return cmd
}

func newServiceStartCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "start <id>",
		Short: "Start the configured service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := o.client().StartService(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printServiceStatus(cmd, o, status)
		},
	}
}

func newServiceStopCmd(o *globalOpts) *cobra.Command {
	var grace int
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop the service (stop signal, grace, then SIGKILL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := o.client().StopService(cmd.Context(), args[0], grace)
			if err != nil {
				return err
			}
			return printServiceStatus(cmd, o, status)
		},
	}
	cmd.Flags().IntVar(&grace, "grace", 0, "override the spec's stop grace (seconds)")
	return cmd
}

func newServiceRestartCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <id>",
		Short: "Restart the service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := o.client().RestartService(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printServiceStatus(cmd, o, status)
		},
	}
}

func newServiceStatusCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "Show the service's supervisor state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := o.client().ServiceStatus(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printServiceStatus(cmd, o, status)
		},
	}
}

func newServiceLogsCmd(o *globalOpts) *cobra.Command {
	var (
		follow  bool
		fromSeq uint64
	)
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print the service's captured stdout/stderr",
		Long: "Print the service's captured output from the agent's in-memory ring " +
			"buffer. With --follow, poll for new output until interrupted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServiceLogs(cmd.Context(), o.client(), cmd, args[0], fromSeq, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep polling for new output")
	cmd.Flags().Uint64Var(&fromSeq, "from-seq", 0, "start from this sequence number")
	return cmd
}

func runServiceLogs(ctx context.Context, cl *client.Client, cmd *cobra.Command, id string, fromSeq uint64, follow bool) error {
	cursor := fromSeq
	warnedGap := false
	for {
		resp, err := cl.ServiceLogs(ctx, id, cursor, 0)
		if err != nil {
			return err
		}
		if cursor < resp.FirstSeq && !warnedGap {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"crucible: %d records evicted from the log ring before this read\n",
				resp.FirstSeq-cursor)
			warnedGap = true
		}
		for _, rec := range resp.Records {
			out := cmd.OutOrStdout()
			if rec.Stream == wire.ServiceLogStderr {
				out = cmd.ErrOrStderr()
			}
			_, _ = out.Write(rec.Data)
		}
		cursor = resp.NextSeq
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if errors.Is(err, context.Canceled) {
				return nil // ^C is how you leave --follow
			}
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func printServiceStatus(cmd *cobra.Command, o *globalOpts, status wire.ServiceStatus) error {
	if o.isJSON() {
		return printJSON(cmd.OutOrStdout(), status)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "state: %s\n", status.State)
	if status.Pid != 0 {
		_, _ = fmt.Fprintf(out, "pid: %d  uptime: %s\n", status.Pid, (time.Duration(status.UptimeMs) * time.Millisecond).Round(time.Second))
	}
	if status.Restarts > 0 {
		_, _ = fmt.Fprintf(out, "policy restarts: %d\n", status.Restarts)
	}
	if le := status.LastExit; le != nil {
		req := "crashed"
		if status.LastExitRequested {
			req = "requested"
		}
		_, _ = fmt.Fprintf(out, "last exit (%s): code %d", req, le.ExitCode)
		if le.Signal != "" {
			_, _ = fmt.Fprintf(out, " signal %s", le.Signal)
		}
		if le.Error != "" {
			_, _ = fmt.Fprintf(out, " error %q", le.Error)
		}
		_, _ = fmt.Fprintln(out)
	}
	if status.Spec != nil {
		_, _ = fmt.Fprintf(out, "cmd: %s\n", strings.Join(status.Spec.Cmd, " "))
	}
	return nil
}
