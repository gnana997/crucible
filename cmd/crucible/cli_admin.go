package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newAdminCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operator commands (daemon backup)",
	}
	cmd.AddCommand(newAdminBackupCmd(o))
	return cmd
}

func newAdminBackupCmd(o *globalOpts) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Download a daemon backup (needs the 'admin_backup' scoped op)",
		Long: "Stream a tar.gz of the daemon's persistent state: the app store, token\n" +
			"file, volume records, and registry credentials, plus a manifest. Volume DATA\n" +
			"is not included — pair with `crucible volume backup`. The archive contains\n" +
			"usable registry secrets; store it like a credential file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var w io.Writer
			path := out
			if path == "" && term.IsTerminal(int(os.Stdout.Fd())) {
				path = "crucible-backup-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
			}
			if path != "" {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				w = f
			} else {
				w = cmd.OutOrStdout() // piped: stream the archive
			}
			if err := o.client().AdminBackup(cmd.Context(), w); err != nil {
				if path != "" {
					_ = os.Remove(path) // don't leave a truncated archive behind
				}
				return err
			}
			if path != "" {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), path)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "write", "w", "", "write the archive to this file (default: crucible-backup-<timestamp>.tar.gz, or stdout when piped)")
	return cmd
}
