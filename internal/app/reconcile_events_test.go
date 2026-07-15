package app

import (
	"testing"

	"github.com/gnana997/crucible/internal/appevents"
	"github.com/gnana997/crucible/sdk/api"
)

func eventSpec(name string) api.AppSpec {
	return api.AppSpec{Name: name, Image: &api.ImageRef{OCI: "nginx:alpine"}, VCPUs: 1, MemoryMiB: 128}
}

// Create and Delete emit intent events (no reconcile loop needed).
func TestEventsCreateDelete(t *testing.T) {
	m, _ := newMgr(t, nil)
	rec, err := m.Create(eventSpec("web"), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	evs, _ := m.Events().Since(0)
	if len(evs) != 1 || evs[0].Type != appevents.TypeCreated || evs[0].App != "web" || evs[0].AppID != rec.ID {
		t.Fatalf("after create: %+v; want one created event for web", evs)
	}
	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	evs, _ = m.Events().Since(0)
	if len(evs) != 2 || evs[1].Type != appevents.TypeDeleted {
		t.Fatalf("after delete: %+v; want created then deleted", evs)
	}
}

// emitPhase emits only on an actual change, tracks from/to, and carries attrs.
func TestEmitPhaseDedup(t *testing.T) {
	m, _ := newMgr(t, nil)
	m.emitPhase("app_x", "x", "sbx1", "running", "boot", nil)
	m.emitPhase("app_x", "x", "sbx1", "running", "reconcile", nil) // unchanged → no event
	m.emitPhase("app_x", "x", "sbx1", "asleep", "sleep", nil)
	m.emitPhase("app_x", "x", "sbx1", "running", "wake", map[string]any{"wake_latency_ms": int64(120)})

	evs, _ := m.Events().Since(0)
	if len(evs) != 3 {
		t.Fatalf("got %d phase events, want 3 (the duplicate must be suppressed)", len(evs))
	}
	wantTo := []string{"running", "asleep", "running"}
	for i, e := range evs {
		if e.Type != appevents.TypePhaseChanged || e.Attrs["to"] != wantTo[i] {
			t.Fatalf("event %d = %+v; want phase_changed to=%s", i, e, wantTo[i])
		}
	}
	if evs[1].Attrs["from"] != "running" {
		t.Errorf("asleep event from = %v, want running", evs[1].Attrs["from"])
	}
	if evs[2].Attrs["wake_latency_ms"] != int64(120) {
		t.Errorf("wake event missing wake_latency_ms: %v", evs[2].Attrs)
	}
}

// The reconcile sweep emits a phase_changed for a converged app once, and is
// idempotent on a second pass with the same phase.
func TestEmitPhaseChangesSweepIdempotent(t *testing.T) {
	m, s := newMgr(t, nil)
	rec, err := m.Create(eventSpec("web"), true)
	if err != nil {
		t.Fatal(err)
	}
	m.obsMu.Lock()
	m.obs[rec.ID] = &observed{instanceID: "sbx1", phase: "running"}
	m.obsMu.Unlock()

	recs, _ := s.List()
	m.emitPhaseChanges(recs)
	m.emitPhaseChanges(recs) // same phase → no second event

	evs, _ := m.Events().Since(0)
	phase := 0
	for _, e := range evs {
		if e.Type == appevents.TypePhaseChanged {
			phase++
			if e.Instance != "sbx1" || e.Attrs["to"] != "running" {
				t.Errorf("sweep event = %+v; want instance sbx1 to=running", e)
			}
		}
	}
	if phase != 1 {
		t.Fatalf("got %d phase_changed events from two sweeps, want exactly 1", phase)
	}
}
