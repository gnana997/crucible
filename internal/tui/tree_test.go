package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/api"
)

func TestRenderTreeGenealogy(t *testing.T) {
	sbs := []api.SandboxResponse{
		{ID: "sbx_root", Profile: "base"},
		{ID: "sbx_f1", SourceSnapshotID: "snap_1", Network: &api.NetworkResponse{Enabled: true, GuestIP: "10.20.0.18"}},
		{ID: "sbx_f2", SourceSnapshotID: "snap_1"},
	}
	snaps := []api.SnapshotResponse{{ID: "snap_1", SourceID: "sbx_root"}}
	out := renderTree(sbs, snaps)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if !strings.Contains(lines[0], "sbx_root") {
		t.Errorf("root should be the first line, got %q", lines[0])
	}
	for _, want := range []string{"◆ snap_1", "sbx_f1", "sbx_f2", "10.20.0.18", "└─", "├─"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing %q\n----\n%s", want, out)
		}
	}
	// The snapshot must appear before its forks (parent before children).
	iSnap := strings.Index(out, "snap_1")
	iFork := strings.Index(out, "sbx_f1")
	if iSnap == -1 || iFork == -1 || iSnap > iFork {
		t.Errorf("snapshot should precede its forks (snap@%d, fork@%d)", iSnap, iFork)
	}
}

func TestRenderTreeEmpty(t *testing.T) {
	if !strings.Contains(renderTree(nil, nil), "no sandboxes yet") {
		t.Error("empty tree should show a friendly placeholder")
	}
}

func TestRenderTreeOrphanSnapshot(t *testing.T) {
	// A snapshot whose source sandbox is already deleted still surfaces.
	snaps := []api.SnapshotResponse{{ID: "snap_x", SourceID: "sbx_gone"}}
	out := renderTree(nil, snaps)
	if !strings.Contains(out, "orphan snapshots") || !strings.Contains(out, "snap_x") {
		t.Errorf("orphan snapshot not surfaced\n%s", out)
	}
}

func TestToggleTreeMode(t *testing.T) {
	m := step(newModel(Config{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	m = step(m, dataMsg{sandboxes: []api.SandboxResponse{{ID: "sbx_root"}}})

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m.mode != modeTree {
		t.Fatal("'t' should switch to tree mode")
	}
	if !strings.Contains(m.View(), "sbx_root") {
		t.Errorf("tree view should render the sandbox; got:\n%s", m.View())
	}
	if !strings.Contains(m.footerView(), "dashboard") {
		t.Error("footer help should offer to switch back to the dashboard")
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m.mode != modeDashboard {
		t.Error("'t' again should switch back to the dashboard")
	}
}
