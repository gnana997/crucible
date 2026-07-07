// Package tui is crucible's live terminal dashboard: a Bubble Tea app that
// polls the daemon through internal/client and renders running sandboxes,
// snapshots, and (in later phases) the fork tree and streaming exec. Like the
// CLI and MCP server it owns no sandbox logic — every view and action is a
// client call, so the dashboard and the CLI can't drift.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/client"
)

// Config wires the dashboard to a daemon.
type Config struct {
	Client *client.Client
	Addr   string // shown in the header
}

const (
	refreshInterval = 2 * time.Second
	requestTimeout  = 5 * time.Second
)

// Run starts the dashboard and blocks until the user quits or ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	p := tea.NewProgram(newModel(cfg), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// --- messages ---------------------------------------------------------------

type dataMsg struct {
	sandboxes []api.SandboxResponse
	snapshots []api.SnapshotResponse
	err       error
}
type whoamiMsg struct{ scope string }
type tickMsg time.Time

// --- model ------------------------------------------------------------------

type model struct {
	cfg           Config
	table         table.Model
	count         int // live sandbox count (for the footer)
	snaps         int
	scope         string
	err           error
	lastRefresh   time.Time
	ready         bool
	width, height int
}

func newModel(cfg Config) model {
	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(accent).BorderBottom(true)
	st.Selected = st.Selected.Bold(true).Foreground(lipgloss.Color("0")).Background(accent)
	t.SetStyles(st)
	return model{cfg: cfg, table: t, scope: "…"}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.fetchWhoami(), tickCmd())
}

func (m model) fetch() tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		sbs, err := cl.ListSandboxes(ctx)
		if err != nil {
			return dataMsg{err: err}
		}
		snaps, err := cl.ListSnapshots(ctx)
		if err != nil {
			return dataMsg{err: err}
		}
		return dataMsg{sandboxes: sbs, snapshots: snaps}
	}
}

func (m model) fetchWhoami() tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		wa, err := cl.Whoami(ctx)
		if err != nil {
			return whoamiMsg{scope: ""} // ignore; the header just omits the scope
		}
		return whoamiMsg{scope: scopeLabel(wa)}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			return m, m.fetch()
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-6))
		m.ready = true
	case tickMsg:
		return m, tea.Batch(m.fetch(), tickCmd())
	case dataMsg:
		m.err = msg.err
		if msg.err == nil {
			m.count = len(msg.sandboxes)
			m.snaps = len(msg.snapshots)
			m.table.SetRows(sandboxRows(msg.sandboxes))
			m.lastRefresh = time.Now()
		}
		return m, nil
	case whoamiMsg:
		if msg.scope != "" {
			m.scope = msg.scope
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if !m.ready {
		return "starting crucible dashboard…"
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), m.table.View(), m.footerView())
}

func (m model) headerView() string {
	title := titleStyle.Render("crucible dashboard")
	meta := metaStyle.Render(fmt.Sprintf("%s · %s", m.cfg.Addr, m.scope))
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(meta)
	if gap < 1 {
		gap = 1
	}
	return title + strings.Repeat(" ", gap) + meta
}

func (m model) footerView() string {
	var status string
	switch {
	case m.err != nil:
		status = errStyle.Render("error: " + m.err.Error())
	case m.lastRefresh.IsZero():
		status = "loading…"
	default:
		status = fmt.Sprintf("%s · %s · updated %s ago",
			plural(m.count, "sandbox", "sandboxes"),
			plural(m.snaps, "snapshot", "snapshots"),
			shortDur(time.Since(m.lastRefresh)))
	}
	help := helpStyle.Render("↑/↓ move · [r]efresh · [q]uit")
	gap := m.width - lipgloss.Width(status) - lipgloss.Width(help)
	if gap < 1 {
		gap = 1
	}
	return metaStyle.Render(status) + strings.Repeat(" ", gap) + help
}
