package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
)

func newVolumeCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "volume",
		Short:   "Manage persistent volumes (durable block devices attached to sandboxes)",
		Aliases: []string{"vol"},
	}
	cmd.AddCommand(newVolumeCreateCmd(o), newVolumeLsCmd(o), newVolumeRmCmd(o),
		newVolumeShredCmd(o), newVolumeRewrapCmd(o), newVolumeKeysCmd(o),
		newVolumeBackupCmd(o), newVolumeRestoreCmd(o), newVolumeCloneCmd(o))
	return cmd
}

func newVolumeRewrapCmd(o *globalOpts) *cobra.Command {
	var toKey, fromKey string
	var all bool
	cmd := &cobra.Command{
		Use:   "rewrap [<name>] --to-key <id>",
		Short: "Re-wrap a volume's encryption key under a different key (rotation; no data is re-encrypted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if toKey == "" {
				return errors.New("--to-key is required")
			}
			cl := o.client()
			if all {
				if fromKey == "" {
					return errors.New("--all requires --from-key <id>")
				}
				n, err := cl.RewrapAllVolumes(cmd.Context(), fromKey, toKey)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rewrapped %d volume(s) from %q to %q\n", n, fromKey, toKey)
				return nil
			}
			if len(args) != 1 {
				return errors.New("give a volume name, or --all --from-key <id>")
			}
			if err := cl.RewrapVolume(cmd.Context(), args[0], toKey); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&toKey, "to-key", "", "keyring id to re-wrap under (required)")
	cmd.Flags().StringVar(&fromKey, "from-key", "", "with --all: only volumes currently on this key")
	cmd.Flags().BoolVar(&all, "all", false, "rewrap every volume on --from-key")
	return cmd
}

func newVolumeKeysCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Manage the volume encryption keyring"}
	cmd.AddCommand(&cobra.Command{
		Use:   "reload",
		Short: "Re-read the daemon's key sources and swap the keyring in without a restart",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.client().ReloadVolumeKeys(cmd.Context()); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "keyring reloaded")
			return nil
		},
	})
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
	cmd.AddCommand(newVolumeBackupLsCmd(o), newVolumeBackupRmCmd(o),
		newVolumeBackupExportCmd(o), newVolumeBackupImportCmd(o))
	return cmd
}

func newVolumeBackupExportCmd(o *globalOpts) *cobra.Command {
	var out string
	var raw bool
	cmd := &cobra.Command{
		Use:   "export <id>",
		Short: "Stream a backup off the host (needs the 'volume_backup' scoped op)",
		Long: "Stream a backup's bytes to a file or stdout so you can ship it off the host\n" +
			"to your own storage. Gzip by default (the image is sparse); --raw streams\n" +
			"the backing file uncompressed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var w io.Writer
			if out != "" {
				f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				w = f
			} else {
				if term.IsTerminal(int(os.Stdout.Fd())) {
					return errors.New("refusing to write a binary backup to a terminal; use -w <file> or pipe it")
				}
				w = cmd.OutOrStdout()
			}
			_, err := o.client().ExportBackup(cmd.Context(), args[0], client.ExportOptions{Raw: raw}, w)
			if err != nil {
				if out != "" {
					_ = os.Remove(out) // don't leave a truncated file
				}
				return err
			}
			if out != "" {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), out)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "write", "w", "", "write the backup to this file (default: stdout when piped)")
	cmd.Flags().BoolVar(&raw, "raw", false, "stream the backing file uncompressed (default: gzip)")
	return cmd
}

func newVolumeBackupImportCmd(o *globalOpts) *cobra.Command {
	var source, consistency, in string
	var raw bool
	cmd := &cobra.Command{
		Use:   "import --source <volume>",
		Short: "Stream a backup onto the host (needs the 'volume_backup' scoped op)",
		Long: "Place an off-host backup onto this host and register it, printing the new\n" +
			"backup id; then `volume restore --from <id> --to <new>` materialises a volume.\n" +
			"Reads a file (-f) or stdin. Expects gzip (as `export` produces) unless --raw.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if source == "" {
				return errors.New("--source <volume> is required")
			}
			var r io.Reader
			if in != "" {
				f, err := os.Open(in)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				r = f
			} else {
				if term.IsTerminal(int(os.Stdin.Fd())) {
					return errors.New("no backup on stdin; use -f <file> or pipe the backup in")
				}
				r = cmd.InOrStdin()
			}
			b, err := o.client().ImportBackup(cmd.Context(), client.ImportOptions{
				SourceVolume: source, Consistency: consistency, Raw: raw,
			}, r)
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
	cmd.Flags().StringVar(&source, "source", "", "the volume this backup came from (recorded on the new backup)")
	cmd.Flags().StringVar(&consistency, "consistency", "", "consistency level to record (default: filesystem)")
	cmd.Flags().StringVarP(&in, "file", "f", "", "read the backup from this file (default: stdin)")
	cmd.Flags().BoolVar(&raw, "raw", false, "the input is uncompressed (default: expect gzip)")
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
	var encrypt, noEncrypt bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a persistent volume (formatted ext4, kept until removed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sizeBytes, err := parseDiskSize(size)
			if err != nil {
				return err
			}
			req := api.CreateVolumeRequest{Name: args[0], SizeBytes: sizeBytes}
			// Send encrypt only when explicitly chosen, so neither flag = the
			// daemon's --volume-encrypt default.
			switch {
			case noEncrypt:
				no := false
				req.Encrypt = &no
			case encrypt:
				yes := true
				req.Encrypt = &yes
			}
			v, err := o.client().CreateVolume(cmd.Context(), req)
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
	cmd.Flags().BoolVar(&encrypt, "encrypt", false, "encrypt this volume at rest with a per-volume LUKS key (needs a daemon master key)")
	cmd.Flags().BoolVar(&noEncrypt, "no-encrypt", false, "force this volume plaintext even when the daemon encrypts new volumes by default")
	cmd.MarkFlagsMutuallyExclusive("encrypt", "no-encrypt")
	return cmd
}

func newVolumeShredCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "shred <name>...",
		Short: "Crypto-shred encrypted volumes: destroy the key so the data is permanently unrecoverable",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, name := range args {
				if err := cl.ShredVolume(cmd.Context(), name); err != nil {
					return fmt.Errorf("shred %s: %w", name, err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
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
			_, _ = fmt.Fprintln(tw, "NAME\tSIZE\tENCRYPTED\tKEY\tATTACHED\tHOST\tAGE")
			for _, v := range vols {
				attached := "-"
				if v.AttachedTo != "" {
					attached = v.AttachedTo
				}
				host := v.HostID
				if host == "" {
					host = "-"
				}
				enc := "no"
				if v.Encrypted {
					enc = "yes"
				}
				key := v.KeyID
				if key == "" {
					key = "-"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, humanSize(v.SizeBytes), enc, key, attached, host, age(v.CreatedAt))
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
