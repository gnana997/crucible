package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ranAndExitedNonZero reports whether err came from an external command
// that actually ran and exited non-zero (an *exec.ExitError) — as opposed
// to a context cancellation, a missing binary, or a netlink/permission
// failure. Teardown idempotency checks require this before trusting a
// tool's stderr text, so a transient or infrastructural error is never
// misread as "the object was already gone" (which would silently leak a
// live nft chain or veth). Neither nft nor ip exposes a distinct exit code
// for "not found", so the specific stderr phrase is still needed as a
// secondary discriminator — but only once we know the command itself ran.
func ranAndExitedNonZero(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

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
