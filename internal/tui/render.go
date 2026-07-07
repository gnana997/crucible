package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/policy"
)

var columns = []table.Column{
	{Title: "SANDBOX", Width: 20},
	{Title: "PROFILE", Width: 14},
	{Title: "AGE", Width: 6},
	{Title: "CPU/MEM", Width: 11},
	{Title: "NETWORK", Width: 26},
	{Title: "FORK", Width: 5},
}

// sandboxRows maps the API list into table rows (newest data each refresh).
func sandboxRows(sbs []api.SandboxResponse) []table.Row {
	rows := make([]table.Row, 0, len(sbs))
	for _, sb := range sbs {
		rows = append(rows, table.Row{
			sb.ID,
			dash(sb.Profile),
			shortDur(time.Since(sb.CreatedAt)),
			fmt.Sprintf("%dc/%dM", sb.VCPUs, sb.MemoryMiB),
			netLabel(sb.Network),
			forkLabel(sb.SourceSnapshotID),
		})
	}
	return rows
}

func netLabel(n *api.NetworkResponse) string {
	if n == nil || !n.Enabled {
		return "—"
	}
	if len(n.Allowlist) > 0 {
		return strings.Join(n.Allowlist, ",")
	}
	return n.GuestIP
}

// forkLabel marks a forked sandbox; the full lineage lives in the tree view.
func forkLabel(sourceSnap string) string {
	if sourceSnap == "" {
		return ""
	}
	return "⑂"
}

// scopeLabel summarizes the token's authority for the header.
func scopeLabel(wa policy.Whoami) string {
	if !wa.Scoped || wa.Policy == nil {
		return "full access"
	}
	if len(wa.Policy.Operations) > 0 {
		ops := make([]string, len(wa.Policy.Operations))
		for i, o := range wa.Policy.Operations {
			ops[i] = string(o)
		}
		return "scoped (" + strings.Join(ops, ",") + ")"
	}
	return "scoped"
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// shortDur renders a duration compactly: 5s, 3m, 2h, 4d.
func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
