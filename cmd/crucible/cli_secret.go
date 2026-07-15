package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newSecretCmd is `crucible secret set|ls|rm` — manage encrypted secret bundles
// (a named set of key→value pairs, injected into an app's env with --secrets).
func newSecretCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted secret bundles (set/list/remove)",
	}
	cmd.AddCommand(newSecretSetCmd(o), newSecretLsCmd(o), newSecretRmCmd(o))
	return cmd
}

func newSecretSetCmd(o *globalOpts) *cobra.Command {
	var (
		envFile  string
		fromFile string
		merge    bool
	)
	cmd := &cobra.Command{
		Use:   "set <name> [KEY]",
		Short: "Create/update a secret bundle (from a .env, or a single key)",
		Long: "Store an encrypted secret bundle. --from-env-file loads a whole .env into the\n" +
			"bundle; otherwise give a KEY and its value comes from --from-file or stdin.\n" +
			"Values never appear on the command line. Inject a bundle into an app with\n" +
			"`app create --secrets <name>` (envFrom).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if envFile != "" {
				if len(args) > 1 {
					return errors.New("give either a KEY or --from-env-file, not both")
				}
				data, err := parseDotenv(envFile)
				if err != nil {
					return err
				}
				if len(data) == 0 {
					return fmt.Errorf("%s has no KEY=VALUE lines", envFile)
				}
				return o.client().SetSecret(cmd.Context(), name, data, merge)
			}
			if len(args) != 2 {
				return errors.New("give a KEY (value on stdin or --from-file), or --from-env-file <path>")
			}
			key := args[1]
			var val []byte
			var err error
			if fromFile != "" {
				val, err = os.ReadFile(fromFile) // exact file bytes
			} else {
				val, err = io.ReadAll(cmd.InOrStdin())
				val = []byte(strings.TrimSuffix(string(val), "\n")) // strip one trailing newline from stdin
			}
			if err != nil {
				return err
			}
			// A single key always merges into any existing bundle.
			return o.client().SetSecret(cmd.Context(), name, map[string]string{key: string(val)}, true)
		},
	}
	cmd.Flags().StringVar(&envFile, "from-env-file", "", "set the whole bundle from a .env file")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read a single KEY's value from a file (else stdin)")
	cmd.Flags().BoolVar(&merge, "merge", false, "with --from-env-file, merge keys into the existing bundle instead of replacing it")
	return cmd
}

func newSecretLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls [name]",
		Short:   "List secret bundles, or one bundle's key names (never values)",
		Aliases: []string{"list"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				keys, err := o.client().SecretKeys(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if o.isJSON() {
					return printJSON(out, keys)
				}
				for _, k := range keys {
					_, _ = fmt.Fprintln(out, k)
				}
				return nil
			}
			names, err := o.client().ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(out, names)
			}
			for _, n := range names {
				_, _ = fmt.Fprintln(out, n)
			}
			return nil
		},
	}
}

func newSecretRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Delete a secret bundle",
		Aliases: []string{"remove"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.client().DeleteSecret(cmd.Context(), args[0])
		},
	}
}

// parseDotenv reads KEY=VALUE lines from a .env file: it skips blank lines and
// `#` comments, strips an optional leading `export `, and unquotes a value
// wrapped in matching single or double quotes. Read client-side; the file itself
// is never uploaded.
func parseDotenv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not KEY=VALUE: %q", path, i+1, raw)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, i+1)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, nil
}
