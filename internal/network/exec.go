package network

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmd executes an external command with CombinedOutput and
// returns a wrapped error that includes the command line and the
// captured stderr/stdout. Centralized so every caller gets
// consistent error messages, and so tests can substitute an
// alternate runner if needed.
//
// Why CombinedOutput: `ip` and `nft` write errors to stderr but
// also emit warnings there; combining makes the returned error
// self-describing in logs without the caller having to stitch
// two streams together.
func runCmd(ctx context.Context, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("network: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("network: %s: %w", strings.Join(argv, " "), err)
		}
		return fmt.Errorf("network: %s: %w: %s", strings.Join(argv, " "), err, trimmed)
	}
	return nil
}

// runCmdStdin is runCmd with an stdin payload, for nft which reads
// rule sets from stdin with `nft -f -`.
func runCmdStdin(ctx context.Context, stdin string, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("network: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("network: %s: %w", strings.Join(argv, " "), err)
		}
		return fmt.Errorf("network: %s: %w: %s", strings.Join(argv, " "), err, trimmed)
	}
	return nil
}

// captureOutput runs a command and returns its stdout on success.
// Used for `ip netns list` and similar read-only queries whose
// output the caller needs to parse.
func captureOutput(ctx context.Context, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("network: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr == "" {
			return "", fmt.Errorf("network: %s: %w", strings.Join(argv, " "), err)
		}
		return "", fmt.Errorf("network: %s: %w: %s", strings.Join(argv, " "), err, stderr)
	}
	return string(out), nil
}
