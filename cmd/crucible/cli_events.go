package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/sdk/api"
)

// eventsPollInterval paces --follow polling.
const eventsPollInterval = time.Second

// newEventsCmd is `crucible events` — the app lifecycle event stream.
func newEventsCmd(o *globalOpts) *cobra.Command {
	var (
		app    string
		since  uint64
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream app lifecycle events (created/booted/slept/woke/crashed/…)",
		Long: "Print recent app lifecycle events and, with --follow, tail new ones. Each\n" +
			"event carries a monotonic cursor (seq); --since resumes after a cursor. A\n" +
			"control plane consumes this for an activity feed and exact sleep/wake timing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvents(cmd, o, app, since, follow)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "filter to a single app by name")
	cmd.Flags().Uint64Var(&since, "since", 0, "resume after this cursor (0 = the events still in the ring)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new events as they arrive")
	return cmd
}

// runEvents prints the recent events (optionally for one app) and, when follow
// is set, polls for new ones from the returned cursor. Shared by `events` and
// `app events`.
func runEvents(cmd *cobra.Command, o *globalOpts, app string, since uint64, follow bool) error {
	out := cmd.OutOrStdout()
	emit := func(e api.AppEvent) error {
		if o.isJSON() {
			return printJSON(out, e)
		}
		renderEvent(out, e)
		return nil
	}
	resp, err := o.client().Events(cmd.Context(), since, app)
	if err != nil {
		return err
	}
	for _, e := range resp.Events {
		if err := emit(e); err != nil {
			return err
		}
	}
	if !follow {
		return nil
	}
	return followEvents(cmd.Context(), o, app, resp.Cursor, emit)
}

func followEvents(ctx context.Context, o *globalOpts, app string, since uint64, emit func(api.AppEvent) error) error {
	cl := o.client()
	t := time.NewTicker(eventsPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		resp, err := cl.Events(ctx, since, app)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, e := range resp.Events {
			if err := emit(e); err != nil {
				return err
			}
		}
		since = resp.Cursor
	}
}

// renderEvent prints one event as a compact aligned line:
//
//	2026-07-15T20:52:03Z  web  phase running→asleep  (sleep)
func renderEvent(w io.Writer, e api.AppEvent) {
	detail := e.Type
	if e.Type == "phase_changed" {
		from, _ := e.Attrs["from"].(string)
		to, _ := e.Attrs["to"].(string)
		detail = "phase " + from + "→" + to
	}
	reason := ""
	if e.Reason != "" {
		reason = "  (" + e.Reason + ")"
	}
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s%s\n", e.Time.Format(time.RFC3339), e.App, detail, reason)
}
