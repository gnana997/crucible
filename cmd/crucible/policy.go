package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/policy"
)

// newPolicyCmd is the policy tooling. `validate` runs the exact same validation
// (`policy.ParseAndValidate`) that gates `daemon token add --policy`, so a file
// checked here can't be rejected at mint time. `show` asks the daemon what the
// current token may actually do (GET /whoami).
func newPolicyCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Author, validate, and inspect scoped-token policies"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate [file]",
		Short: "Validate a token policy JSON file (omit the file or pass - to read stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readPolicyInput(cmd, args)
			if err != nil {
				return err
			}
			if _, err := policy.ParseAndValidate(data); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "policy OK")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the effective policy for the current token (asks the daemon)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wa, err := o.client().Whoami(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), wa)
			}
			if !wa.Scoped || wa.Policy == nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "full access (unscoped token or loopback daemon)")
				return nil
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "scoped token — effective policy:")
			return printJSON(cmd.OutOrStdout(), wa.Policy)
		},
	})
	return cmd
}

// readPolicyInput reads the policy from the named file, or stdin when the arg is
// omitted or "-".
func readPolicyInput(cmd *cobra.Command, args []string) ([]byte, error) {
	if len(args) == 0 || args[0] == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(args[0])
}
