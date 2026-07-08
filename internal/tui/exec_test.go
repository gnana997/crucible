package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/client"
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

// TestScrollbackRetainsPriorCommands is the M2 scrollback property: a
// finished command's block stays in the buffer when the next one runs, so
// the whole session's history shows up (not just the last run).
func TestScrollbackRetainsPriorCommands(t *testing.T) {
	m := openDetail(t)

	// First command block, driven through the event stream to completion.
	m.execCh = make(chan execEvent, 8)
	m.execing = true
	m.appendExec("$ echo one\n")
	m = step(m, execEvent{out: []byte("one\n")})
	m = step(m, execEvent{done: true, res: agentwire.ExecResult{ExitCode: 0}})

	// Second command block appends; the first must survive.
	m.execCh = make(chan execEvent, 8)
	m.execing = true
	m.appendExec("$ echo two\n")
	m = step(m, execEvent{out: []byte("two\n")})
	m = step(m, execEvent{done: true, res: agentwire.ExecResult{ExitCode: 0}})

	for _, want := range []string{"echo one", "one", "echo two", "two"} {
		if !strings.Contains(m.execOut, want) {
			t.Errorf("scrollback missing %q:\n%s", want, m.execOut)
		}
	}
}

// TestAttachEnterEchoesAndSendsStdin: while attached, pressing enter echoes
// the typed line into the scrollback (no PTY echo) and writes it to the
// shell's stdin pipe.
func TestAttachEnterEchoesAndSendsStdin(t *testing.T) {
	m := openDetail(t)
	pr, pw := io.Pipe()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := pr.Read(buf)
		got <- string(buf[:n])
	}()

	m.attached = true
	m.execing = true
	m.attachIn = pw
	m.input.SetValue("ls -la")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	if !strings.Contains(m.execOut, "ls -la") {
		t.Errorf("attach enter should echo the command into scrollback; execOut=%q", m.execOut)
	}
	if cmd == nil {
		t.Fatal("attach enter should return a sendStdin command")
	}
	cmd() // perform the pipe write

	select {
	case s := <-got:
		if strings.TrimSpace(s) != "ls -la" {
			t.Errorf("stdin = %q, want %q", s, "ls -la")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing written to the shell's stdin")
	}
}

// TestAttachDoneResetsState: when the shell session ends, the model drops out
// of attached mode and clears the stdin pipe so a new session can start.
func TestAttachDoneResetsState(t *testing.T) {
	m := openDetail(t)
	_, pw := io.Pipe()
	m.attached = true
	m.execing = true
	m.attachIn = pw
	m.execCh = make(chan execEvent, 4)

	m = step(m, execEvent{done: true, res: agentwire.ExecResult{ExitCode: 0}})

	if m.attached {
		t.Error("attached should be false after the shell exits")
	}
	if m.attachIn != nil {
		t.Error("attachIn should be nil after the shell exits")
	}
	if m.execing {
		t.Error("execing should be false after the shell exits")
	}
	if !strings.Contains(m.execOut, "exit 0") {
		t.Errorf("exit summary missing from scrollback: %q", m.execOut)
	}
}

// TestTabAttachBlockedWhileOneShotStreaming: tab must not start a shell while
// a one-shot exec is still in flight (single active session).
func TestTabAttachBlockedWhileOneShotStreaming(t *testing.T) {
	m := openDetail(t)
	m.execing = true // a one-shot is streaming
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.attached {
		t.Error("tab should not attach a shell while a one-shot is streaming")
	}
}

// deadClient points at a refused port so attachShell's goroutine fails fast
// (a clean done event) instead of panicking on a nil client — lets us test
// the synchronous attach state transitions without a live daemon.
func deadClient() *client.Client { return client.New("127.0.0.1:1") }

// TestTabInDetailAttachesShell proves tab actually attaches in the detail
// view (the earlier blocked-path test couldn't distinguish "handled" from
// "ignored").
func TestTabInDetailAttachesShell(t *testing.T) {
	m := openDetail(t)
	m.cfg.Client = deadClient()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	defer m.closeAttach()

	if !m.attached {
		t.Error("tab in detail should attach an interactive shell")
	}
	if m.attachIn == nil {
		t.Error("attachIn should be set after attaching")
	}
	if cmd == nil {
		t.Error("attach should return a waitExec command")
	}
	if !strings.Contains(m.execOut, "interactive shell") {
		t.Errorf("attach banner missing from scrollback: %q", m.execOut)
	}
}

// TestTabFromListOpensDetailAndAttaches proves the list shortcut: tab on a
// selected sandbox opens its detail view AND starts a shell in one step.
func TestTabFromListOpensDetailAndAttaches(t *testing.T) {
	m := step(newModel(Config{Addr: "http://x", Client: deadClient()}), tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, dataMsg{sandboxes: []api.SandboxResponse{
		{ID: "sbx_a", Profile: "base", VCPUs: 1, MemoryMiB: 256, CreatedAt: time.Now()},
	}})
	if m.mode != modeDashboard {
		t.Fatalf("precondition: expected dashboard, mode = %d", m.mode)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	defer m.closeAttach()

	if m.mode != modeDetail {
		t.Errorf("tab on the list should open the detail view, mode = %d", m.mode)
	}
	if m.selected.ID != "sbx_a" {
		t.Errorf("selected = %q, want sbx_a", m.selected.ID)
	}
	if !m.attached {
		t.Error("tab on the list should drop straight into a shell")
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
