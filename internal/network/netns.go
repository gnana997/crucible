package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// netnsRoot is where iproute2 places bind-mounted netns pseudo-files.
// Every netns created by `ip netns add <name>` appears as a file at
// /var/run/netns/<name>; that file's path is what jailer's --netns
// flag wants, and what unix.Setns opens for the DHCP responder.
const netnsRoot = "/var/run/netns"

// NetnsPrefix scopes every crucible-managed netns under a
// recognizable name. Operators can list `ip netns list | grep
// crucible-` to see exactly what's ours, and startup orphan reap
// deletes everything with this prefix.
const NetnsPrefix = "crucible-"

// validNetnsName mirrors iproute2's own validation: alphanumeric
// + hyphen + underscore, length 1..64. We apply it up front so
// invalid names surface a clear Go error instead of an opaque
// `ip netns` exit.
var validNetnsName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// NetnsPath returns the canonical file path for a netns name,
// regardless of whether the netns currently exists. Callers pass
// this to jailer (--netns) and to unix.Setns.
func NetnsPath(name string) string {
	return filepath.Join(netnsRoot, name)
}

// CreateNetns creates a named network namespace via `ip netns add`.
// Returns an error if name fails validation or if the command
// fails (most commonly: name already exists, or not running as
// root).
func CreateNetns(ctx context.Context, name string) error {
	if !validNetnsName.MatchString(name) {
		return fmt.Errorf("network: invalid netns name %q", name)
	}
	return runCmd(ctx, "ip", "netns", "add", name)
}

// DeleteNetns removes a named netns via `ip netns delete`. The
// kernel garbage-collects veth pairs, bridges, and TAP devices
// that lived only in that netns — we don't need to delete them
// individually.
//
// Missing-netns is treated as success (idempotent teardown).
func DeleteNetns(ctx context.Context, name string) error {
	if !validNetnsName.MatchString(name) {
		return fmt.Errorf("network: invalid netns name %q", name)
	}
	// Short-circuit: if the pseudo-file is absent the netns is
	// already gone. Saves a subprocess spawn + an error that we'd
	// have to parse.
	if _, err := os.Stat(NetnsPath(name)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return runCmd(ctx, "ip", "netns", "delete", name)
}

// ListCrucibleNetns returns every netns name that starts with
// NetnsPrefix. Used by the daemon's startup orphan reap.
//
// Shells out to `ip netns list` because its output format is
// stable and documented. Directly reading /var/run/netns/ would
// work too but couples us to iproute2's storage layout.
func ListCrucibleNetns(ctx context.Context) ([]string, error) {
	cmd, err := captureOutput(ctx, "ip", "netns", "list")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(cmd, "\n") {
		// Each line looks like "name (id: N)" or just "name".
		// Trim annotations and take the first whitespace-separated
		// token.
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.Fields(line)[0]
		if strings.HasPrefix(name, NetnsPrefix) {
			out = append(out, name)
		}
	}
	return out, nil
}
