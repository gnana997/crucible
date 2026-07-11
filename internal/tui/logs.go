package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/gnana997/crucible/sdk/api"
)

// logsMsg carries a batch of durable-log records fetched for the logs view.
type logsMsg struct {
	id      string
	records []api.LogRecord
	offset  int64
	err     error
}

// openLogs switches to the logs view for a sandbox and kicks off the first read
// (the full history; subsequent polls follow from the returned cursor).
func (m model) openLogs(id string) (tea.Model, tea.Cmd) {
	m.mode = modeLogs
	m.logsID = id
	m.logsOffset = -1
	m.logsContent = ""
	m.logsVP.SetContent(metaStyle.Render("loading logs for " + id + "…"))
	m.logsVP.GotoTop()
	return m, m.fetchLogs(id, -1)
}

// fetchLogs reads durable logs for id from the byte cursor `since` (-1 = from
// the start) across both sources, returning them as a logsMsg.
func (m model) fetchLogs(id string, since int64) tea.Cmd {
	cl := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		resp, err := cl.Logs(ctx, id, since, "all")
		if err != nil {
			return logsMsg{id: id, err: err}
		}
		return logsMsg{id: id, records: resp.Records, offset: resp.NextOffset}
	}
}

// handleLogsMsg appends newly-read records to the logs viewport and advances the
// follow cursor. A batch for a sandbox we've since navigated away from is
// dropped. The view stays pinned to the tail unless the user scrolled up.
func (m model) handleLogsMsg(msg logsMsg) (tea.Model, tea.Cmd) {
	if msg.id != m.logsID {
		return m, nil
	}
	if msg.err != nil {
		m.logsVP.SetContent(errStyle.Render("logs: " + msg.err.Error()))
		return m, nil
	}
	m.logsOffset = msg.offset
	if len(msg.records) == 0 {
		if m.logsContent == "" {
			m.logsVP.SetContent(metaStyle.Render("(no logs yet — service output and exec activity appear here)"))
		}
		return m, nil
	}
	atBottom := m.logsVP.AtBottom()
	rendered := renderLogLines(msg.records)
	if m.logsContent == "" {
		m.logsContent = rendered
	} else {
		m.logsContent += "\n" + rendered
	}
	m.logsVP.SetContent(m.logsContent)
	if atBottom {
		m.logsVP.GotoBottom()
	}
	return m, nil
}

// updateLogsKey handles keys while the logs view is open. esc returns to the
// dashboard; q quits; everything else scrolls the viewport.
func (m model) updateLogsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeDashboard
		m.logsID, m.logsContent = "", ""
		return m, nil
	case "q":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.logsVP, cmd = m.logsVP.Update(msg)
	return m, cmd
}

// logsBody renders the logs view: a title, a divider, and the scrolling pane.
func (m model) logsBody() string {
	title := titleStyle.Render("logs · " + m.logsID)
	div := dividerStyle.Render(strings.Repeat("─", max(1, m.width)))
	return lipgloss.JoinVertical(lipgloss.Left, title, div, m.logsVP.View())
}

// renderLogLines formats a batch of records: a dim timestamp per line, with
// stderr in the error colour and synthesized events dimmed.
func renderLogLines(recs []api.LogRecord) string {
	var b strings.Builder
	for i, r := range recs {
		if i > 0 {
			b.WriteByte('\n')
		}
		ts := time.UnixMilli(r.TimeMs).Format("15:04:05")
		b.WriteString(metaStyle.Render(ts+" ") + styledLogText(r))
	}
	return b.String()
}

func styledLogText(r api.LogRecord) string {
	txt := strings.TrimRight(r.Text, "\n")
	switch r.Stream {
	case "stderr":
		return errStyle.Render(txt)
	case "event":
		return metaStyle.Render(txt)
	default:
		return txt
	}
}
