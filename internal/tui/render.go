package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/policy"
)

// columns is the initial layout; columnsFor recomputes it for the real terminal
// width on the first WindowSizeMsg.
var columns = columnsFor(100)

// columnsFor sizes the table to the terminal: the minor columns are fixed and
// SANDBOX/NETWORK absorb the slack, so the FORK column never spills off-screen
// on a narrow terminal. bubbles' table adds 1 space of padding on each side of
// every column, so the usable content budget is width - 2*ncols.
func columnsFor(width int) []table.Column {
	const ncols = 6
	inner := width - 2*ncols
	if inner < 34 { // floor so titles stay legible on a very small terminal
		inner = 34
	}
	age, cpu, fork := 4, 9, 4
	profile := 12
	flex := inner - age - cpu - fork - profile
	if flex < 22 { // reclaim space from PROFILE before starving the flex columns
		profile = 8
		flex = inner - age - cpu - fork - profile
	}
	sandbox := 20
	if sandbox > flex-7 {
		sandbox = flex - 7
	}
	if sandbox < 12 {
		sandbox = 12
	}
	network := flex - sandbox
	if network < 7 { // keep the "NETWORK" header from truncating
		network = 7
	}
	return []table.Column{
		{Title: "SANDBOX", Width: sandbox},
		{Title: "PROFILE", Width: profile},
		{Title: "AGE", Width: age},
		{Title: "CPU/MEM", Width: cpu},
		{Title: "NETWORK", Width: network},
		{Title: "FORK", Width: fork},
	}
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
