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
	cmd.AddCommand(newVolumeCreateCmd(o), newVolumeLsCmd(o), newVolumeRmCmd(o))
	return cmd
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
