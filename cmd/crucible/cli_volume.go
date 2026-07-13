package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/sdk/api"
)

func newVolumeCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "volume",
		Short:   "Manage persistent volumes (durable block devices attached to sandboxes)",
		Aliases: []string{"vol"},
	}
	cmd.AddCommand(newVolumeCreateCmd(o), newVolumeLsCmd(o), newVolumeRmCmd(o),
		newVolumeBackupCmd(o), newVolumeRestoreCmd(o), newVolumeCloneCmd(o))
	return cmd
}

// newVolumeRestoreCmd is `volume restore --from <backup-id> --to <new-volume>`.
func newVolumeRestoreCmd(o *globalOpts) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "restore --from <backup-id> --to <new-volume>",
		Short: "Restore a backup into a new volume (never overwrites an existing one)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := o.client().RestoreBackup(cmd.Context(), from, to)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), v)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), v.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "backup id to restore (required)")
	cmd.Flags().StringVar(&to, "to", "", "name of the new volume to create (required)")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// newVolumeCloneCmd is `volume clone <src> <dst>`.
func newVolumeCloneCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "clone <src> <dst>",
		Short: "Copy a quiescent volume into a new volume (detached or slept source)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := o.client().CloneVolume(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), v)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), v.Name)
			return nil
		},
	}
}

// newVolumeBackupCmd is `volume backup <name>` (create a backup) plus `ls`/`rm`
// subcommands. A first arg that isn't a subcommand is the volume to back up.
func newVolumeBackupCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup <name>",
		Short: "Back up a volume (point-in-time copy; restore to a new volume with `volume restore`)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := o.client().BackupVolume(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), b)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), b.ID)
			return nil
		},
	}
	cmd.AddCommand(newVolumeBackupLsCmd(o), newVolumeBackupRmCmd(o))
	return cmd
}

func newVolumeBackupLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls [<volume>]",
		Short:   "List volume backups (optionally for one volume)",
		Aliases: []string{"list"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var vol string
			if len(args) == 1 {
				vol = args[0]
			}
			page, err := o.client().ListBackups(cmd.Context(), vol)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), page.Items)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "ID\tVOLUME\tSIZE\tCONSISTENCY\tAGE")
			for _, b := range page.Items {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.ID, b.SourceVolume, humanSize(b.SizeBytes), b.Consistency, age(b.CreatedAt))
			}
			return tw.Flush()
		},
	}
}

func newVolumeBackupRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>...",
		Short:   "Remove volume backups by id",
		Aliases: []string{"delete"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, id := range args {
				if err := cl.DeleteBackup(cmd.Context(), id); err != nil {
					return fmt.Errorf("delete %s: %w", id, err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
}

func newVolumeCreateCmd(o *globalOpts) *cobra.Command {
	var size string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a persistent volume (formatted ext4, kept until removed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sizeBytes, err := parseDiskSize(size)
			if err != nil {
				return err
			}
			v, err := o.client().CreateVolume(cmd.Context(), api.CreateVolumeRequest{Name: args[0], SizeBytes: sizeBytes})
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), v)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), v.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&size, "size", "", "volume size, e.g. 5G or 512M (default: the daemon's --volume-default-size)")
	return cmd
}

func newVolumeLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List persistent volumes",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			page, err := o.client().ListVolumes(cmd.Context())
			if err != nil {
				return err
			}
			vols := page.Items
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), vols)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "NAME\tSIZE\tATTACHED\tHOST\tAGE")
			for _, v := range vols {
				attached := "-"
				if v.AttachedTo != "" {
					attached = v.AttachedTo
				}
				host := v.HostID
				if host == "" {
					host = "-"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, humanSize(v.SizeBytes), attached, host, age(v.CreatedAt))
			}
			return tw.Flush()
		},
	}
}

func newVolumeRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>...",
		Short:   "Remove persistent volumes and their data (refused while attached)",
		Aliases: []string{"delete"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, name := range args {
				if err := cl.DeleteVolume(cmd.Context(), name); err != nil {
					return fmt.Errorf("delete %s: %w", name, err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
}

// humanSize renders a byte count as a short binary-unit string (e.g. 512M, 5G).
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.4g%c", float64(b)/float64(div), "KMGTPE"[exp])
}
