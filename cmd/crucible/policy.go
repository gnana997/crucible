package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/policy"
)

// newPolicyCmd is the operator-side policy tooling. `crucible policy validate`
// runs the exact same validation (`policy.ParseAndValidate`) that gates
// `daemon token add --policy`, so an operator can check a policy file before
// minting a scoped key — and the two can never disagree.
func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Author and validate scoped-token policies"}
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
