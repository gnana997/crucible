package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/api"
)

func TestLogsViewOpenAppendAndBack(t *testing.T) {
	m := dashboardWith(t) // one selectable sandbox: sbx_a

	// 'l' opens the logs view for the selected sandbox.
	m = step(m, key("l"))
	if m.mode != modeLogs || m.logsID != "sbx_a" {
		t.Fatalf("after 'l': mode=%v logsID=%q, want modeLogs/sbx_a", m.mode, m.logsID)
	}

	// A batch of records renders and advances the follow cursor.
	now := time.Now().UnixMilli()
	m = step(m, logsMsg{id: "sbx_a", offset: 42, records: []api.LogRecord{
		{TimeMs: now, Source: "exec", Stream: "stdout", Text: "hello-from-logs"},
		{TimeMs: now, Source: "service", Stream: "stderr", Text: "a-warning"},
	}})
	if m.logsOffset != 42 {
		t.Errorf("logsOffset = %d, want 42", m.logsOffset)
	}
	view := m.View()
	if !strings.Contains(view, "hello-from-logs") || !strings.Contains(view, "logs · sbx_a") {
		t.Errorf("logs view missing content:\n%s", view)
	}

	// A batch for a sandbox we're not viewing is dropped.
	before := m.logsContent
	m = step(m, logsMsg{id: "sbx_other", records: []api.LogRecord{{Text: "should-not-appear"}}})
	if m.logsContent != before {
		t.Errorf("a stale batch for another sandbox was applied")
	}

	// esc returns to the dashboard and clears logs state.
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeDashboard || m.logsID != "" {
		t.Errorf("after esc: mode=%v logsID=%q, want dashboard/empty", m.mode, m.logsID)
	}
}
