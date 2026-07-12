package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newRegistryCmd is the `crucible registry` command group: manage the
// credentials the daemon uses to pull private images. Credentials live on the
// daemon, so a durable app on a private image survives a restart (the
// reconciler re-pulls with the stored credential).
func newRegistryCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage credentials for pulling private images",
		Long: "Store per-registry credentials the daemon uses to pull private images " +
			"(for run, app create, and app self-heal/redeploy). Credentials live on the " +
			"daemon — not read from your local ~/.docker/config.json — so a durable app " +
			"on a private image keeps working across a daemon restart.",
	}
	cmd.AddCommand(newRegistryLoginCmd(o), newRegistryLogoutCmd(o), newRegistryListCmd(o))
	return cmd
}

func newRegistryLoginCmd(o *globalOpts) *cobra.Command {
	var (
		username      string
		password      string
		passwordStdin bool
	)
	cmd := &cobra.Command{
		Use:   "login <host>",
		Short: "Store a credential for a private registry",
		Long: "Store the credential the daemon uses to pull from a private registry — " +
			"e.g. ghcr.io, quay.io, docker.io, a self-hosted registry[:port], or the " +
			"static-credential form of a cloud registry (GCP `_json_key`, Azure ACR " +
			"service principal). The secret is sent to the daemon and never printed back.\n\n" +
			"Provide the secret with --password-stdin (recommended and scriptable, e.g. " +
			"`echo $TOKEN | crucible registry login ghcr.io -u USER --password-stdin`), " +
			"--password, or an interactive prompt.\n\n" +
			"AWS ECR: log in with `aws ecr get-login-password | crucible registry login " +
			"<acct>.dkr.ecr.<region>.amazonaws.com -u AWS --password-stdin`; the token " +
			"expires in ~12h, so re-run it periodically.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			secret, err := resolveRegistrySecret(cmd, password, passwordStdin)
			if err != nil {
				return err
			}
			if err := o.client().RegistryLogin(cmd.Context(), args[0], username, secret); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Stored credential for %s\n", args[0])
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&username, "username", "u", "", "registry username (may be empty for token-only registries)")
	f.StringVarP(&password, "password", "p", "", "registry password or token (insecure — prefer --password-stdin)")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the password/token from stdin")
	return cmd
}

// resolveRegistrySecret gets the secret from --password-stdin, --password, or a
// masked interactive prompt, mirroring `docker login`.
func resolveRegistrySecret(cmd *cobra.Command, password string, stdin bool) (string, error) {
	if stdin {
		if password != "" {
			return "", fmt.Errorf("--password and --password-stdin are mutually exclusive")
		}
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		s := strings.TrimRight(string(b), "\r\n")
		if s == "" {
			return "", fmt.Errorf("no password on stdin")
		}
		return s, nil
	}
	if password != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"WARNING: --password exposes the secret in your shell history; prefer --password-stdin")
		return password, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("no password provided (use --password-stdin or --password)")
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(b) == 0 {
		return "", fmt.Errorf("no password entered")
	}
	return string(b), nil
}

func newRegistryLogoutCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "logout <host>",
		Short: "Remove a stored registry credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.client().RegistryLogout(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed credential for %s\n", args[0])
			return nil
		},
	}
}

func newRegistryListCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List stored registry credentials (host + username; never the secret)",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := o.client().ListRegistryCredentials(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), creds)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "HOST\tUSERNAME\tADDED")
			for _, c := range creds {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Host, orDash(c.Username), c.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	return cmd
}
