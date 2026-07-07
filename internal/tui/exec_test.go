package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
)

// openDetail drives the model to the detail view for the first sandbox row.
func openDetail(t *testing.T) model {
	t.Helper()
	m := step(newModel(Config{Addr: "http://x"}), tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, dataMsg{sandboxes: []api.SandboxResponse{
		{ID: "sbx_a", Profile: "base", VCPUs: 1, MemoryMiB: 256, CreatedAt: time.Now()},
	}})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeDetail {
		t.Fatalf("enter on a selected row should open detail, mode = %d", m.mode)
	}
	if !m.input.Focused() {
		t.Fatal("detail view should focus the command input")
	}
	return m
}

func TestEnterOpensDetailEscReturns(t *testing.T) {
	m := openDetail(t)
	if m.selected.ID != "sbx_a" {
		t.Errorf("selected = %q, want sbx_a", m.selected.ID)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeDashboard {
		t.Errorf("esc should return to dashboard, mode = %d", m.mode)
	}
	if m.input.Focused() {
		t.Error("input should blur on leaving detail")
	}
}

func TestDetailEscDoesNotQuit(t *testing.T) {
	m := openDetail(t)
	// In detail mode 'q' is a literal keystroke for the command line, not quit.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("'q' in detail mode must type, not quit")
		}
	}
}

// TestExecEventStream feeds the streaming events the exec goroutine would send
// and checks the model accumulates output and finishes cleanly.
func TestExecEventStream(t *testing.T) {
	m := openDetail(t)
	m.execCh = make(chan execEvent, 8)
	m.execing = true

	m = step(m, execEvent{out: []byte("hello\n")})
	if !strings.Contains(m.execOut, "hello") {
		t.Errorf("stdout not accumulated: %q", m.execOut)
	}
	m = step(m, execEvent{errb: []byte("oops\n")})
	if !strings.Contains(m.execOut, "oops") {
		t.Errorf("stderr not accumulated: %q", m.execOut)
	}
	m = step(m, execEvent{done: true, res: agentwire.ExecResult{ExitCode: 0, DurationMs: 12}})
	if m.execing {
		t.Error("execing should be false after the done event")
	}
	if !strings.Contains(m.execOut, "exit 0") {
		t.Errorf("exit summary missing: %q", m.execOut)
	}
}

func TestExitLine(t *testing.T) {
	if got := exitLine(agentwire.ExecResult{ExitCode: 0, DurationMs: 5}, nil); !strings.Contains(got, "exit 0") {
		t.Errorf("ok exit = %q", got)
	}
	if got := exitLine(agentwire.ExecResult{ExitCode: 1, DurationMs: 5}, nil); !strings.Contains(got, "exit 1") {
		t.Errorf("bad exit = %q", got)
	}
	if got := exitLine(agentwire.ExecResult{ExitCode: -1, Signal: "SIGKILL"}, nil); !strings.Contains(got, "SIGKILL") {
		t.Errorf("signal missing from = %q", got)
	}
	if got := exitLine(agentwire.ExecResult{}, errTest); !strings.Contains(got, "error") {
		t.Errorf("transport failure = %q", got)
	}
}

func TestSandboxDetailShowsLineage(t *testing.T) {
	sb := api.SandboxResponse{ID: "sbx_b", Profile: "python", VCPUs: 2, MemoryMiB: 512, SourceSnapshotID: "snap_1", CreatedAt: time.Now()}
	out := sandboxDetail(sb)
	for _, want := range []string{"sbx_b", "python", "2c/512M", "snap_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q\n%s", want, out)
		}
	}
}
