package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gnana997/crucible/internal/policy"
)

// This file holds the operator-policy enforcement — the guardrails from the
// Config. Each helper is a pure check the handlers call before touching the
// daemon, so an agent can never operate outside the policy set at launch.

// toolEnabled reports whether a tool should be exposed: it must pass the
// allowlist (when set) and not be on the deny list.
func (c Config) toolEnabled(name string) bool {
	if len(c.Tools) > 0 && !slices.Contains(c.Tools, name) {
		return false
	}
	return !slices.Contains(c.DenyTools, name)
}

// resolveProfile applies the default profile and enforces the allowlist,
// returning the profile the daemon should launch.
func (c Config) resolveProfile(p string) (string, error) {
	if p == "" {
		p = c.DefaultProfile
	}
	if len(c.AllowProfiles) > 0 {
		if p == "" {
			return "", fmt.Errorf("a profile is required; allowed: %s", strings.Join(c.AllowProfiles, ", "))
		}
		if !slices.Contains(c.AllowProfiles, p) {
			return "", fmt.Errorf("profile %q is not allowed; allowed: %s", p, strings.Join(c.AllowProfiles, ", "))
		}
	}
	return p, nil
}

// checkNetAllow enforces the --net-allow-max ceiling: every requested host must
// appear in it (exact match). No ceiling means the agent may name any host.
func (c Config) checkNetAllow(hosts []string) error {
	if len(c.NetAllowMax) == 0 {
		return nil
	}
	for _, h := range hosts {
		if !slices.Contains(c.NetAllowMax, h) {
			return fmt.Errorf("host %q is not permitted by this server's --net-allow-max", h)
		}
	}
	return nil
}

// clampTimeout bounds a command timeout (seconds) by --max-timeout. An
// unbounded (<=0) or over-limit request is pulled down to the ceiling.
func (c Config) clampTimeout(sec int) int {
	max := int(c.MaxTimeout / time.Second)
	if max <= 0 {
		return sec
	}
	if sec <= 0 || sec > max {
		return max
	}
	return sec
}

// checkFork normalizes the fork count (0 → 1) and enforces --max-fork.
func (c Config) checkFork(count int) (int, error) {
	if count <= 0 {
		count = 1
	}
	if c.MaxFork > 0 && count > c.MaxFork {
		return 0, fmt.Errorf("count %d exceeds this server's --max-fork limit of %d", count, c.MaxFork)
	}
	return count, nil
}

// checkCapacity refuses a create/fork that would push the daemon past
// --max-sandboxes. It is best-effort: the live count is read just before the
// create, so two concurrent creates could still race past the limit.
func (c Config) checkCapacity(ctx context.Context, want int) error {
	if c.MaxSandboxes <= 0 {
		return nil
	}
	sbs, err := c.Client.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	if len(sbs.Items)+want > c.MaxSandboxes {
		return fmt.Errorf("would exceed this server's --max-sandboxes limit of %d (%d already live)", c.MaxSandboxes, len(sbs.Items))
	}
	return nil
}

// toolOps is the set of daemon operations a tool performs. The MCP server uses
// it to mirror the token policy — a tool is advertised only when the policy
// permits every operation it would need. (run cleans up after itself, so it
// needs delete too.)
func toolOps(name string) []policy.Operation {
	switch name {
	case "run":
		return []policy.Operation{policy.OpCreate, policy.OpExec, policy.OpDelete}
	case "create_sandbox":
		return []policy.Operation{policy.OpCreate}
	case "exec":
		return []policy.Operation{policy.OpExec}
	case "write_files":
		// Writing files into a sandbox is exec-grade power over the guest.
		return []policy.Operation{policy.OpExec}
	case "read_file":
		return []policy.Operation{policy.OpRead}
	case "stop_sandbox":
		// The daemon gates service/stop as an exec-class mutation.
		return []policy.Operation{policy.OpExec}
	case "logs":
		return []policy.Operation{policy.OpRead}
	case "snapshot":
		return []policy.Operation{policy.OpSnapshot}
	case "fork":
		return []policy.Operation{policy.OpFork}
	case "delete_sandbox", "delete_snapshot":
		return []policy.Operation{policy.OpDelete}
	case "list_sandboxes", "inspect_sandbox", "list_snapshots", "list_profiles":
		return []policy.Operation{policy.OpRead}
	}
	return nil
}

// policyPermitsTool reports whether pol allows every operation the named tool
// performs. A nil policy (unscoped token, or a whoami that failed) permits all.
func policyPermitsTool(pol *policy.Policy, name string) bool {
	if pol == nil {
		return true
	}
	for _, op := range toolOps(name) {
		if !pol.Allows(op) {
			return false
		}
	}
	return true
}
