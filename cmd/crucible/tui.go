package main

import (
	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/tui"
)

// newTuiCmd launches the live terminal dashboard. Like every other client
// command it talks to the daemon at --addr with --token, so it works against a
// local or a remote daemon and is bounded by a scoped token just the same.
func newTuiCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Live terminal dashboard for sandboxes, snapshots, and fork trees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tui.Run(cmd.Context(), tui.Config{Client: o.client(), Addr: o.addr})
		},
	}
}
