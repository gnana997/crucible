package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/api"
)

// dashboardWith returns a ready dashboard model holding one selectable sandbox.
func dashboardWith(t *testing.T, snaps ...api.SnapshotResponse) model {
	t.Helper()
	m := step(newModel(Config{Addr: "http://x"}), tea.WindowSizeMsg{Width: 120, Height: 30})
	return step(m, dataMsg{
		sandboxes: []api.SandboxResponse{{ID: "sbx_a", CreatedAt: time.Now()}},
		snapshots: snaps,
	})
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestCreateActionDispatches(t *testing.T) {
	m := dashboardWith(t)
	m2, cmd := m.Update(key("c"))
	mm := m2.(model)
	if !mm.busy {
		t.Error("create should mark the model busy")
	}
	if cmd == nil {
		t.Error("create should dispatch a command")
	}
	if !strings.Contains(mm.notice, "create") {
		t.Errorf("notice = %q, want a create notice", mm.notice)
	}
}

func TestSnapshotAndForkGating(t *testing.T) {
	// fork with no snapshot for the sandbox -> a helpful error, no dispatch.
	m := dashboardWith(t)
	m2, cmd := m.Update(key("f"))
	mm := m2.(model)
	if cmd != nil || !mm.noticeErr || !strings.Contains(mm.notice, "snapshot") {
		t.Errorf("fork w/o snapshot: cmd=%v notice=%q", cmd != nil, mm.notice)
	}

	// with a snapshot of the sandbox, fork dispatches.
	m = dashboardWith(t, api.SnapshotResponse{ID: "snap_1", SourceID: "sbx_a", CreatedAt: time.Now()})
	_, cmd = m.Update(key("f"))
	if cmd == nil {
		t.Error("fork should dispatch when a snapshot exists")
	}
}

func TestScopeBlocksAction(t *testing.T) {
	m := dashboardWith(t)
	// read-only scope: create/snapshot/fork/delete all forbidden.
	m = step(m, whoamiMsg{ok: true, wa: policy.Whoami{
		Scoped: true, Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead}},
	}})
	for _, k := range []string{"c", "s"} {
		m2, cmd := m.Update(key(k))
		mm := m2.(model)
		if cmd != nil {
			t.Errorf("%q should be blocked (no dispatch)", k)
		}
		if !mm.noticeErr || !strings.Contains(mm.notice, "not permitted") {
			t.Errorf("%q notice = %q, want 'not permitted'", k, mm.notice)
		}
	}
	// the disallowed actions render struck-through in the help row.
	if !m.can(policy.OpRead) {
		t.Error("read-only scope should still allow read")
	}
	if m.can(policy.OpCreate) {
		t.Error("read-only scope must forbid create")
	}
}

func TestDeleteConfirmFlow(t *testing.T) {
	m := dashboardWith(t)
	// 'd' arms the confirm prompt; it does not delete yet.
	m2, cmd := m.Update(key("d"))
	mm := m2.(model)
	if !mm.confirming || mm.confirmID != "sbx_a" || cmd != nil {
		t.Fatalf("d should arm confirm: confirming=%v id=%q cmd=%v", mm.confirming, mm.confirmID, cmd != nil)
	}
	// 'n' cancels.
	m3, cmd := mm.Update(key("n"))
	mmm := m3.(model)
	if mmm.confirming || cmd != nil || !strings.Contains(mmm.notice, "cancel") {
		t.Errorf("n should cancel: confirming=%v notice=%q", mmm.confirming, mmm.notice)
	}
	// re-arm, then 'y' dispatches the delete.
	m4, _ := mm.Update(key("y"))
	mmmm := m4.(model)
	if mmmm.confirming || !mmmm.busy {
		t.Errorf("y should dispatch delete: confirming=%v busy=%v", mmmm.confirming, mmmm.busy)
	}
}

func TestActionResultClearsBusyAndRefreshes(t *testing.T) {
	m := dashboardWith(t)
	m.busy = true
	m2, cmd := m.Update(actionMsg{verb: "create", detail: "sbx_z"})
	mm := m2.(model)
	if mm.busy {
		t.Error("busy should clear on the action result")
	}
	if mm.noticeErr || !strings.Contains(mm.notice, "sbx_z") {
		t.Errorf("notice = %q, want the new id", mm.notice)
	}
	if cmd == nil {
		t.Error("a successful action should trigger a refresh fetch")
	}

	// a failed action surfaces the error and does not refresh.
	m3, cmd := m.Update(actionMsg{verb: "fork", err: errTest})
	mmm := m3.(model)
	if !mmm.noticeErr || cmd != nil {
		t.Errorf("failed action: noticeErr=%v cmd=%v", mmm.noticeErr, cmd != nil)
	}
}

// TestSelectionSurvivesEmptyThenPopulated guards the cursor-reanchor fix: a
// table that was empty (its cursor invalidated by navigation) must present a
// valid selection once sandboxes appear, or every selection-based action would
// silently no-op.
func TestSelectionSurvivesEmptyThenPopulated(t *testing.T) {
	m := step(newModel(Config{Addr: "x"}), tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, dataMsg{sandboxes: nil}) // empty list
	// navigating an empty table drives the cursor out of range
	m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	m = step(m, dataMsg{sandboxes: []api.SandboxResponse{{ID: "sbx_a", CreatedAt: time.Now()}}})
	if sb, ok := m.selectedSandbox(); !ok || sb.ID != "sbx_a" {
		t.Fatalf("selection lost after list became non-empty: ok=%v id=%q", ok, sb.ID)
	}
}

func TestLatestSnapshotOf(t *testing.T) {
	base := time.Now()
	snaps := []api.SnapshotResponse{
		{ID: "old", SourceID: "sbx_a", CreatedAt: base},
		{ID: "new", SourceID: "sbx_a", CreatedAt: base.Add(time.Minute)},
		{ID: "other", SourceID: "sbx_b", CreatedAt: base.Add(time.Hour)},
	}
	got, ok := latestSnapshotOf(snaps, "sbx_a")
	if !ok || got.ID != "new" {
		t.Errorf("latest = %q ok=%v, want 'new'", got.ID, ok)
	}
	if _, ok := latestSnapshotOf(snaps, "sbx_z"); ok {
		t.Error("no snapshot for sbx_z should report not-found")
	}
}
