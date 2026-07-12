package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSleepWakeInPlaceRoundTrip exercises the Manager-level orchestration of
// B3/B4 against the stub runner: sleep snapshots + stops the VMM while KEEPING
// the record, and wake restores in place (reusing the same workdir) and clears
// the asleep marker. The real KVM behavior (RAM freed, netns kept, listener
// survives) is covered by scripts/spike_sleepwake.sh.
func TestSleepWakeInPlaceRoundTrip(t *testing.T) {
	m, r := newTestManager(t)
	ctx := context.Background()

	s, err := m.Create(ctx, CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.asleep != nil {
		t.Fatal("fresh sandbox should not be asleep")
	}

	// --- Sleep -------------------------------------------------------------
	if err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("SleepInPlace: %v", err)
	}
	if s.asleep == nil {
		t.Fatal("after sleep: asleep marker not set")
	}
	// The snapshot artifacts live under the sandbox's own workdir (kept, not
	// removed) so wake can restore in place.
	for _, p := range []string{s.asleep.statePath, s.asleep.memPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("sleep artifact missing: %v", err)
		}
	}
	// The record is KEPT — a slept sandbox is still addressable.
	if got, err := m.Get(s.ID); err != nil || got != s {
		t.Fatalf("slept sandbox not retained in registry: got=%v err=%v", got, err)
	}
	if len(r.restoreCalls) != 0 {
		t.Fatalf("sleep should not restore anything; got %d restore calls", len(r.restoreCalls))
	}

	// --- Wake --------------------------------------------------------------
	if err := m.WakeInPlace(ctx, s.ID); err != nil {
		t.Fatalf("WakeInPlace: %v", err)
	}
	if s.asleep != nil {
		t.Fatal("after wake: asleep marker not cleared")
	}
	if len(r.restoreCalls) != 1 {
		t.Fatalf("wake should restore exactly once; got %d", len(r.restoreCalls))
	}
	// Wake restores IN PLACE: same workdir, same rootfs path, no new netns.
	rc := r.restoreCalls[0]
	if rc.Workdir != s.Workdir {
		t.Errorf("restore workdir = %q, want the sandbox's own %q", rc.Workdir, s.Workdir)
	}
	if want := filepath.Join(s.Workdir, perSandboxRootfsName); rc.RootfsPath != want {
		t.Errorf("restore rootfs = %q, want the persistent %q", rc.RootfsPath, want)
	}
	if !rc.LazyMem {
		t.Error("wake should restore with LazyMem")
	}
	// The woken sandbox has a fresh agent channel (the stub runner served one).
	if s.execClient == nil || s.VSockPath == "" {
		t.Error("wake did not install a fresh agent channel")
	}

	// Waking an already-awake sandbox is an error.
	if err := m.WakeInPlace(ctx, s.ID); err == nil || !strings.Contains(err.Error(), "not asleep") {
		t.Fatalf("second wake err = %v, want a 'not asleep' error", err)
	}
}

// TestSleepUsesFreshDirAndGCsPrevious guards the fix for the jailer-EACCES bug:
// every sleep must write a FRESH snapshot dir (reusing one path fails on the
// second sleep after a wake), and the previous dir is reclaimed so exactly one
// snapshot is kept per instance.
func TestSleepUsesFreshDirAndGCsPrevious(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep 1: %v", err)
	}
	dir1 := s.snapDir
	if dir1 == "" {
		t.Fatal("snapDir not set after first sleep")
	}
	if _, err := os.Stat(dir1); err != nil {
		t.Fatalf("dir1 missing after sleep: %v", err)
	}

	// Wake keeps dir1 — the woken VM's lazy-memory pager reads its memory file.
	if err := m.WakeInPlace(ctx, s.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if s.snapDir != dir1 {
		t.Fatalf("wake changed snapDir to %q, want it kept at %q", s.snapDir, dir1)
	}
	if _, err := os.Stat(dir1); err != nil {
		t.Fatalf("wake removed the live snapshot dir: %v", err)
	}

	// Second sleep → a FRESH dir, and dir1 is GC'd.
	if err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep 2: %v", err)
	}
	if s.snapDir == dir1 {
		t.Fatal("second sleep reused the same dir (the jailer-EACCES bug)")
	}
	if _, err := os.Stat(s.snapDir); err != nil {
		t.Fatalf("dir2 missing after second sleep: %v", err)
	}
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Fatalf("previous snapshot dir not GC'd (stat err=%v)", err)
	}
}
