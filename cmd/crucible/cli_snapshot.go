package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSnapshotCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshot",
		Short:   "Manage snapshots",
		Aliases: []string{"snap"},
	}
	cmd.AddCommand(
		newSnapshotCreateCmd(o),
		newSnapshotLsCmd(o),
		newSnapshotInspectCmd(o),
		newSnapshotRmCmd(o),
	)
	return cmd
}

func newSnapshotCreateCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "create <sandbox-id>",
		Short: "Snapshot a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			snap, err := o.client().Snapshot(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), snap)
			}
			fmt.Fprintln(cmd.OutOrStdout(), snap.ID)
			return nil
		},
	}
}

func newSnapshotLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List snapshots",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snaps, err := o.client().ListSnapshots(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), snaps)
			}
			tw := newTable(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tSOURCE\tVCPUS\tMEM(MiB)\tAGE")
			for _, s := range snaps {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", s.ID, s.SourceID, s.VCPUs, s.MemoryMiB, age(s.CreatedAt))
			}
			return tw.Flush()
		},
	}
}

func newSnapshotInspectCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "Show a snapshot's full details (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			snap, err := o.client().GetSnapshot(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), snap)
		},
	}
}

func newSnapshotRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>...",
		Short:   "Delete one or more snapshots",
		Aliases: []string{"delete"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, id := range args {
				if err := cl.DeleteSnapshot(cmd.Context(), id); err != nil {
					return fmt.Errorf("delete %s: %w", id, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
}
