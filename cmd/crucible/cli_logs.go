package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/sdk/api"
)

// logsPollInterval paces --follow polling, matching the daemon's drain
// cadence so a follower lags the live log by at most ~one interval.
const logsPollInterval = time.Second

func newLogsCmd(o *globalOpts) *cobra.Command {
	var (
		follow bool
		source string
	)
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show a sandbox's logs (service output and exec activity)",
		Long: "Print a sandbox's durable logs. --follow (-f) tails new output; " +
			"--source filters to the service (entrypoint) output or exec activity. " +
			"Logs persist after the sandbox is gone, so a crashed workload can still be inspected.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch source {
			case "", "all", "service", "exec":
			default:
				return fmt.Errorf("--source must be service, exec, or all")
			}
			return runLogs(cmd, o, args[0], source, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log output as it arrives")
	cmd.Flags().StringVar(&source, "source", "all", "filter logs: service|exec|all")
	return cmd
}

// runLogs prints a sandbox's recent logs and, when follow is set, tails
// new output. Shared by `sandbox logs` and `app logs`.
func runLogs(cmd *cobra.Command, o *globalOpts, id, source string, follow bool) error {
	switch source {
	case "", "all", "service", "exec":
	default:
		return fmt.Errorf("--source must be service, exec, or all")
	}
	out := cmd.OutOrStdout()
	resp, err := o.client().Logs(cmd.Context(), id, -1, source)
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
	return followLogs(cmd.Context(), o, id, source, resp.NextOffset, out)
}

// followLogs polls from the given cursor, printing new records until the
// context is cancelled (Ctrl-C).
func followLogs(ctx context.Context, o *globalOpts, id, source string, since int64, out io.Writer) error {
	cl := o.client()
	ticker := time.NewTicker(logsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		resp, err := cl.Logs(ctx, id, since, source)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, rec := range resp.Records {
			renderLogRecord(out, rec)
		}
		since = resp.NextOffset
	}
}

// renderLogRecord prints one record. Output streams are written raw so
// concatenation reproduces the original byte stream; synthesized events
// (exec start/exit, ring gaps) are set off on their own line.
func renderLogRecord(w io.Writer, rec api.LogRecord) {
	if rec.Stream == "event" {
		_, _ = fmt.Fprintf(w, "== %s ==\n", rec.Text)
		return
	}
	_, _ = fmt.Fprint(w, rec.Text)
}
