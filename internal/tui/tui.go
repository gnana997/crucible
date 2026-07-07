// Package tui is crucible's live terminal dashboard: a Bubble Tea app that
// polls the daemon through internal/client and renders running sandboxes,
// snapshots, the fork tree, and streaming exec. Like the CLI and MCP server it
// owns no sandbox logic — every view and action is a client call, so the
// dashboard and the CLI can't drift.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
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

type viewMode int

const (
	modeDashboard viewMode = iota
	modeTree
	modeDetail
)

type model struct {
	cfg   Config
	table table.Model
	vp    viewport.Model // fork-tree view
	mode  viewMode

	sandboxes []api.SandboxResponse
	snapshots []api.SnapshotResponse
	count     int
	snaps     int
	scope     string
	err       error

	// detail + streaming exec
	selected api.SandboxResponse
	input    textinput.Model
	execVP   viewport.Model
	execOut  string
	execing  bool
	execCh   chan execEvent

	lastRefresh   time.Time
	ready         bool
	width, height int
}

func newModel(cfg Config) model {
	t := table.New(table.WithColumns(columns), table.WithFocused(true), table.WithHeight(10))
	t.SetStyles(newTableStyles())

	in := textinput.New()
	in.Prompt = promptStyle.Render("$ ")
	in.Placeholder = "type a command, enter to run"
	in.CharLimit = 512

	return model{cfg: cfg, table: t, vp: viewport.New(0, 0), execVP: viewport.New(0, 0), input: in}
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
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.mode == modeDetail {
			return m.updateDetailKey(msg)
		}
		return m.updateMainKey(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-6))
		m.vp.Width, m.vp.Height = msg.Width, max(3, msg.Height-6)
		m.execVP.Width, m.execVP.Height = msg.Width, max(3, msg.Height-9)
		m.input.Width = max(10, msg.Width-6)
		m.ready = true

	case tickMsg:
		return m, tea.Batch(m.fetch(), tickCmd())

	case dataMsg:
		m.err = msg.err
		if msg.err == nil {
			m.sandboxes, m.snapshots = msg.sandboxes, msg.snapshots
			m.count, m.snaps = len(msg.sandboxes), len(msg.snapshots)
			m.table.SetRows(sandboxRows(msg.sandboxes))
			if m.mode == modeTree {
				m.vp.SetContent(renderTree(m.sandboxes, m.snapshots))
			}
			m.lastRefresh = time.Now()
		}
		return m, nil

	case whoamiMsg:
		if msg.scope != "" {
			m.scope = msg.scope
		}
		return m, nil

	case execEvent:
		return m.handleExecEvent(msg)
	}

	// residual messages (navigation, cursor blink) go to the active view.
	var cmd tea.Cmd
	switch m.mode {
	case modeTree:
		m.vp, cmd = m.vp.Update(msg)
	case modeDetail:
		m.input, cmd = m.input.Update(msg)
	default:
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

func (m model) updateMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "r":
		return m, m.fetch()
	case "t":
		if m.mode == modeTree {
			m.mode = modeDashboard
		} else {
			m.mode = modeTree
			m.vp.SetContent(renderTree(m.sandboxes, m.snapshots))
			m.vp.GotoTop()
		}
		return m, nil
	case "enter":
		if m.mode == modeDashboard {
			if sb, ok := m.selectedSandbox(); ok {
				m.selected = sb
				m.mode = modeDetail
				m.execOut = ""
				m.execVP.SetContent(metaStyle.Render("run a command in this sandbox — type below and press enter"))
				m.input.Reset()
				return m, m.input.Focus()
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.mode == modeTree {
		m.vp, cmd = m.vp.Update(msg)
	} else {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

func (m model) updateDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeDashboard
		m.input.Blur()
		return m, nil
	case "enter":
		cmdline := strings.TrimSpace(m.input.Value())
		if cmdline == "" || m.execing {
			return m, nil
		}
		ch := make(chan execEvent, 64)
		m.execCh = ch
		m.execing = true
		m.execOut = promptStyle.Render("$ "+cmdline) + "\n"
		m.execVP.SetContent(m.execOut)
		m.input.Reset()
		go runExec(m.cfg.Client, m.selected.ID, cmdline, ch)
		return m, waitExec(ch)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) handleExecEvent(e execEvent) (tea.Model, tea.Cmd) {
	if len(e.out) > 0 {
		m.execOut += string(e.out)
	}
	if len(e.errb) > 0 {
		m.execOut += stderrStyle.Render(string(e.errb))
	}
	if e.done {
		m.execing = false
		m.execOut += "\n" + exitLine(e.res, e.fail) + "\n"
		m.execVP.SetContent(m.execOut)
		m.execVP.GotoBottom()
		return m, nil
	}
	m.execVP.SetContent(m.execOut)
	m.execVP.GotoBottom()
	return m, waitExec(m.execCh)
}

func (m model) selectedSandbox() (api.SandboxResponse, bool) {
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return api.SandboxResponse{}, false
	}
	for _, sb := range m.sandboxes {
		if sb.ID == row[0] {
			return sb, true
		}
	}
	return api.SandboxResponse{}, false
}

func (m model) View() string {
	if !m.ready {
		return "starting crucible dashboard…"
	}
	var body string
	switch m.mode {
	case modeTree:
		body = m.vp.View()
	case modeDetail:
		body = m.detailBody()
	default:
		body = m.table.View()
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), body, m.footerView())
}

func (m model) detailBody() string {
	div := dividerStyle.Render(strings.Repeat("─", max(20, m.width)))
	return lipgloss.JoinVertical(lipgloss.Left, sandboxDetail(m.selected), div, m.execVP.View(), m.input.View())
}

func (m model) headerView() string {
	title := titleStyle.Render("crucible dashboard")
	meta := metaStyle.Render(m.cfg.Addr)
	if m.scope != "" {
		meta += metaStyle.Render(" · ") + scopeStyled(m.scope)
	}
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
		status = metaStyle.Render("loading…")
	default:
		status = metaStyle.Render(fmt.Sprintf("%s · %s · updated %s ago",
			plural(m.count, "sandbox", "sandboxes"),
			plural(m.snaps, "snapshot", "snapshots"),
			shortDur(time.Since(m.lastRefresh))))
	}
	help := helpStyle.Render(m.helpText())
	gap := m.width - lipgloss.Width(status) - lipgloss.Width(help)
	if gap < 1 {
		gap = 1
	}
	return status + strings.Repeat(" ", gap) + help
}

func (m model) helpText() string {
	switch m.mode {
	case modeTree:
		return "↑/↓ scroll · [t] dashboard · [r]efresh · [q]uit"
	case modeDetail:
		return "type a command · enter run · esc back · ctrl+c quit"
	default:
		return "↑/↓ move · enter detail · [t]ree · [r]efresh · [q]uit"
	}
}

// scopeStyled colors the header access-state: full access is a heads-up (warn
// gold), a scoped policy is informational (cyan).
func scopeStyled(scope string) string {
	if scope == "full access" {
		return warnStyle.Render(scope)
	}
	return altStyle.Render(scope)
}
