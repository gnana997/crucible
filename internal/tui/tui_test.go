package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/api"
)

// step applies a message and returns the concrete model back.
func step(m model, msg tea.Msg) model {
	updated, _ := m.Update(msg)
	return updated.(model)
}

func TestUpdateDataPopulatesTable(t *testing.T) {
	m := step(newModel(Config{Addr: "http://x"}), tea.WindowSizeMsg{Width: 100, Height: 30})
	if !m.ready {
		t.Fatal("model should be ready after a window-size message")
	}
	m = step(m, dataMsg{
		sandboxes: []api.SandboxResponse{
			{ID: "sbx_a", Profile: "python-3.12", VCPUs: 1, MemoryMiB: 512, CreatedAt: time.Now()},
			{ID: "sbx_b", SourceSnapshotID: "snap_1", CreatedAt: time.Now()},
		},
		snapshots: []api.SnapshotResponse{{ID: "snap_1"}},
	})
	if m.count != 2 || m.snaps != 1 {
		t.Errorf("count/snaps = %d/%d, want 2/1", m.count, m.snaps)
	}
	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if rows[0][0] != "sbx_a" || rows[0][1] != "python-3.12" {
		t.Errorf("row0 = %v", rows[0])
	}
	if rows[1][5] != "⑂" {
		t.Errorf("forked sandbox FORK col = %q, want the fork mark", rows[1][5])
	}
	if m.lastRefresh.IsZero() {
		t.Error("lastRefresh should be set after a successful data msg")
	}
}

func TestUpdateDataError(t *testing.T) {
	m := step(newModel(Config{}), dataMsg{err: errTest})
	if m.err == nil {
		t.Error("error should be recorded")
	}
}

func TestUpdateQuitKeys(t *testing.T) {
	for _, key := range []string{"q", "esc"} {
		_, cmd := newModel(Config{}).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			t.Fatalf("%q should return a command", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%q should quit", key)
		}
	}
}

func TestUpdateWhoamiSetsScope(t *testing.T) {
	wa := policy.Whoami{Scoped: true, Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead}}}
	m := step(newModel(Config{}), whoamiMsg{wa: wa, ok: true})
	if m.scope != "scoped (read)" {
		t.Errorf("scope = %q, want 'scoped (read)'", m.scope)
	}
	// a failed whoami (ok=false) must not clobber a known scope.
	m = step(m, whoamiMsg{ok: false})
	if m.scope != "scoped (read)" {
		t.Errorf("failed whoami should not overwrite scope; got %q", m.scope)
	}
}

func TestScopeLabel(t *testing.T) {
	if got := scopeLabel(policy.Whoami{Scoped: false}); got != "full access" {
		t.Errorf("unscoped = %q", got)
	}
	scoped := policy.Whoami{Scoped: true, Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead, policy.OpExec}}}
	if got := scopeLabel(scoped); got != "scoped (read,exec)" {
		t.Errorf("scoped w/ ops = %q", got)
	}
	if got := scopeLabel(policy.Whoami{Scoped: true, Policy: &policy.Policy{}}); got != "scoped" {
		t.Errorf("scoped no ops = %q", got)
	}
}

func TestShortDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"}, {90 * time.Second, "1m"}, {2 * time.Hour, "2h"}, {50 * time.Hour, "2d"},
	} {
		if got := shortDur(c.d); got != c.want {
			t.Errorf("shortDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestNetAndForkLabels(t *testing.T) {
	if netLabel(nil) != "—" {
		t.Error("nil network should be a dash")
	}
	if netLabel(&api.NetworkResponse{Enabled: true, Allowlist: []string{"pypi.org"}}) != "pypi.org" {
		t.Error("allowlist net label")
	}
	if netLabel(&api.NetworkResponse{Enabled: true, GuestIP: "10.0.0.2"}) != "10.0.0.2" {
		t.Error("guest-ip net label")
	}
	if forkLabel("") != "" {
		t.Error("non-forked should have no mark")
	}
	if forkLabel("snap_1") != "⑂" {
		t.Error("forked should carry the mark")
	}
}

var errTest = tea.ErrProgramKilled

// TestViewFitsWidth guards responsiveness: at any terminal width, no rendered
// line may spill past the right edge (which is what cut the FORK column and the
// footer help before). Checks dashboard, tree, and detail modes.
func TestViewFitsWidth(t *testing.T) {
	longIDs := []api.SandboxResponse{{
		ID: "sbx_verylongidentifier0", Profile: "python-3.12", VCPUs: 2, MemoryMiB: 1024,
		CreatedAt: time.Now(), Network: &api.NetworkResponse{Enabled: true, GuestIP: "10.0.0.2"},
	}}
	for _, w := range []int{40, 60, 80, 100, 140} {
		base := step(newModel(Config{Addr: "http://127.0.0.1:7878"}), tea.WindowSizeMsg{Width: w, Height: 20})
		base = step(base, whoamiMsg{ok: true, wa: policy.Whoami{Scoped: false}})
		base = step(base, dataMsg{sandboxes: longIDs})

		views := map[string]model{"dashboard": base}
		views["tree"] = step(base, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
		views["detail"] = step(base, tea.KeyMsg{Type: tea.KeyEnter})

		for mode, m := range views {
			for _, line := range strings.Split(m.View(), "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("width=%d %s: line width %d exceeds terminal:\n%q", w, mode, lw, line)
				}
			}
		}
	}
}

func TestViewRendersDashboard(t *testing.T) {
	m := step(newModel(Config{Addr: "http://127.0.0.1:7878"}), tea.WindowSizeMsg{Width: 100, Height: 20})
	m = step(m, whoamiMsg{wa: policy.Whoami{Scoped: false}, ok: true})
	m = step(m, dataMsg{
		sandboxes: []api.SandboxResponse{{ID: "sbx_abc", Profile: "base", VCPUs: 1, MemoryMiB: 256, CreatedAt: time.Now()}},
	})
	out := m.View()
	for _, want := range []string{"crucible dashboard", "http://127.0.0.1:7878", "full access", "SANDBOX", "sbx_abc", "1 sandbox", "[q]uit"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() is missing %q\n----\n%s", want, out)
		}
	}
}
