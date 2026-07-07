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
	"github.com/gnana997/crucible/internal/policy"
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
type whoamiMsg struct {
	wa policy.Whoami
	ok bool // false when the whoami call failed; the header keeps its prior scope
}
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
	whoami    policy.Whoami
	err       error

	// actions
	notice     string // transient status-line feedback from the last action
	noticeErr  bool
	busy       bool   // a mutating action is in flight
	confirming bool   // awaiting y/n for a destructive action
	confirmID  string // the sandbox a pending delete targets

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
			return whoamiMsg{ok: false} // ignore; the header just omits the scope
		}
		return whoamiMsg{wa: wa, ok: true}
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
		if m.confirming {
			return m.updateConfirmKey(msg)
		}
		if m.mode == modeDetail {
			return m.updateDetailKey(msg)
		}
		return m.updateMainKey(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(columnsFor(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-4))
		m.vp.Width, m.vp.Height = msg.Width, max(3, msg.Height-4)
		m.execVP.Width, m.execVP.Height = msg.Width, max(3, msg.Height-7)
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
			// SetRows doesn't restore the cursor, so a table that was empty (or
			// shrank below the cursor) is left with nothing selected — which would
			// silently disable every selection-based action. Re-anchor it.
			if len(msg.sandboxes) > 0 && len(m.table.SelectedRow()) == 0 {
				m.table.SetCursor(0)
			}
			if m.mode == modeTree {
				m.vp.SetContent(renderTree(m.sandboxes, m.snapshots))
			}
			m.lastRefresh = time.Now()
		}
		return m, nil

	case whoamiMsg:
		if msg.ok {
			m.whoami = msg.wa
			m.scope = scopeLabel(msg.wa)
		}
		return m, nil

	case actionMsg:
		m.busy = false
		if msg.err != nil {
			m.notice, m.noticeErr = msg.verb+" failed: "+msg.err.Error(), true
			return m, nil
		}
		m.notice, m.noticeErr = actionOK(msg), false
		return m, m.fetch() // reflect the change without waiting for the next tick

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
	case "c":
		return m.startAction(policy.OpCreate, "create", m.createCmd())
	case "s":
		if sb, ok := m.selectedSandbox(); ok {
			return m.startAction(policy.OpSnapshot, "snapshot", m.snapshotCmd(sb.ID))
		}
		return m, nil
	case "f":
		sb, ok := m.selectedSandbox()
		if !ok {
			return m, nil
		}
		snap, has := latestSnapshotOf(m.snapshots, sb.ID)
		if !has {
			m.notice, m.noticeErr = "fork needs a snapshot — press [s] first", true
			return m, nil
		}
		return m.startAction(policy.OpFork, "fork", m.forkCmd(snap.ID))
	case "d":
		sb, ok := m.selectedSandbox()
		if !ok {
			return m, nil
		}
		if !m.can(policy.OpDelete) {
			m.notice, m.noticeErr = "delete not permitted by policy scope", true
			return m, nil
		}
		m.confirming, m.confirmID, m.notice = true, sb.ID, ""
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

// startAction gates a mutating action on the token's scope and prevents a second
// action while one is in flight, then dispatches its command.
func (m model) startAction(op policy.Operation, verb string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if !m.can(op) {
		m.notice, m.noticeErr = verb+" not permitted by policy scope", true
		return m, nil
	}
	if m.busy {
		return m, nil
	}
	m.busy = true
	m.notice, m.noticeErr = verb+"…", false
	return m, cmd
}

// updateConfirmKey handles the y/n prompt for a pending destructive action.
func (m model) updateConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		id := m.confirmID
		m.confirming, m.confirmID = false, ""
		m.busy = true
		m.notice, m.noticeErr = "delete…", false
		return m, m.deleteCmd(id)
	default:
		m.confirming, m.confirmID = false, ""
		m.notice, m.noticeErr = "delete cancelled", false
		return m, nil
	}
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
		if !m.can(policy.OpExec) {
			m.execOut += errStyle.Render("exec not permitted by policy scope") + "\n"
			m.execVP.SetContent(m.execOut)
			m.execVP.GotoBottom()
			m.input.Reset()
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
	full := lipgloss.JoinVertical(lipgloss.Left, m.headerView(), body, m.footerView())
	// JoinVertical pads every block to the widest line, so a single long line
	// (e.g. exec output or a long id in the un-wrapping viewport) would push the
	// whole frame past the terminal edge. Clamp every line as a final guarantee.
	return clampBlock(full, m.width)
}

func clampBlock(s string, width int) string {
	if width < 1 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = clampLine(l, width)
	}
	return strings.Join(lines, "\n")
}

func (m model) detailBody() string {
	div := dividerStyle.Render(strings.Repeat("─", max(1, m.width)))
	return lipgloss.JoinVertical(lipgloss.Left, sandboxDetail(m.selected), div, m.execVP.View(), m.input.View())
}

func (m model) headerView() string {
	title := titleStyle.Render("crucible dashboard")
	meta := metaStyle.Render(m.cfg.Addr)
	if m.scope != "" {
		meta += metaStyle.Render(" · ") + scopeStyled(m.scope)
	}
	return clampLine(spread(title, meta, m.width), m.width)
}

func (m model) footerView() string {
	status, help := m.statusLine(), m.helpLine(false)
	// If the roomy help won't fit alongside the status, drop to the compact
	// variant before letting the clamp truncate anything.
	if lipgloss.Width(status)+lipgloss.Width(help)+1 > m.width {
		help = m.helpLine(true)
	}
	return clampLine(spread(status, help, m.width), m.width)
}

func (m model) statusLine() string {
	switch {
	case m.confirming:
		return warnStyle.Render("delete "+m.confirmID+"?") + " " + helpStyle.Render("[y]es  [n]o")
	case m.notice != "":
		if m.noticeErr {
			return errStyle.Render(m.notice)
		}
		return okStyle.Render(m.notice)
	case m.err != nil:
		return errStyle.Render("error: " + m.err.Error())
	case m.lastRefresh.IsZero():
		return metaStyle.Render("loading…")
	default:
		return metaStyle.Render(fmt.Sprintf("%s · %s · updated %s ago",
			plural(m.count, "sandbox", "sandboxes"),
			plural(m.snaps, "snapshot", "snapshots"),
			shortDur(time.Since(m.lastRefresh))))
	}
}

// helpLine returns the key hints for the current mode; compact drops the
// lower-priority hints so the essentials survive on a narrow terminal.
func (m model) helpLine(compact bool) string {
	var s string
	switch m.mode {
	case modeTree:
		if compact {
			s = "[t] back · [q]uit"
		} else {
			s = "↑/↓ scroll · [t] dashboard · [r]efresh · [q]uit"
		}
	case modeDetail:
		if compact {
			s = "enter run · esc back"
		} else {
			s = "type a command · enter run · esc back · ctrl+c quit"
		}
	default:
		if compact {
			s = m.actionsHelp() + " · [q]uit"
		} else {
			s = "↑/↓ move · enter detail · " + m.actionsHelp() + " · [t]ree · [r] · [q]uit"
		}
	}
	return helpStyle.Render(s)
}

// spread places left and right on one line filled to width; clampLine caps a
// (possibly styled) line to width with ANSI-aware truncation so nothing spills
// past the terminal edge.
func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func clampLine(s string, width int) string {
	if width < 1 {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// actionsHelp renders the mutating actions, striking through any the token's
// scope forbids so the gating is visible before a key is pressed.
func (m model) actionsHelp() string {
	items := []struct {
		label string
		op    policy.Operation
	}{
		{"[c]reate", policy.OpCreate},
		{"[s]nap", policy.OpSnapshot},
		{"[f]ork", policy.OpFork},
		{"[d]el", policy.OpDelete},
	}
	parts := make([]string, len(items))
	for i, it := range items {
		if m.can(it.op) {
			parts[i] = it.label
		} else {
			parts[i] = disabledStyle.Render(it.label)
		}
	}
	return strings.Join(parts, " ")
}

// scopeStyled colors the header access-state: full access is a heads-up (warn
// gold), a scoped policy is informational (cyan).
func scopeStyled(scope string) string {
	if scope == "full access" {
		return warnStyle.Render(scope)
	}
	return altStyle.Render(scope)
}
