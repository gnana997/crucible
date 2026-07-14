package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSleepWakeInPlaceRoundTrip exercises the Manager-level orchestration of
// Exercised against the stub runner: sleep snapshots + stops the VMM while KEEPING
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
	if _, err := m.SleepInPlace(ctx, s.ID); err != nil {
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

// TestWakeAdmissionRefusesWhenMemoryLow: a wake is refused when host
// MemAvailable is below the configured floor, and admitted once it recovers.
func TestWakeAdmissionRefusesWhenMemoryLow(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	avail := 1000
	m.cfg.WakeMinFreeMiB = 512
	m.cfg.MemAvailableMiB = func() (int, error) { return avail, nil }

	s, err := m.Create(ctx, CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep: %v", err)
	}

	// Host is starved → refuse.
	avail = 100
	if err := m.WakeInPlace(ctx, s.ID); !errors.Is(err, ErrInsufficientMemory) {
		t.Fatalf("wake under low memory err = %v, want ErrInsufficientMemory", err)
	}
	// Still asleep and retryable.
	m.mu.RLock()
	stillAsleep := s.asleep != nil
	m.mu.RUnlock()
	if !stillAsleep {
		t.Fatal("refused wake should leave the sandbox asleep")
	}

	// Memory recovers → admit.
	avail = 1000
	if err := m.WakeInPlace(ctx, s.ID); err != nil {
		t.Fatalf("wake after memory recovered: %v", err)
	}
}

// TestSleepAdmissionRefusesWhenDiskLow: a sleep (snapshot) is refused when free
// disk under WorkBase is below the floor, the app stays running (not asleep,
// still routable), and it is admitted once disk recovers.
func TestSleepAdmissionRefusesWhenDiskLow(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	free := 4096
	m.cfg.SleepMinFreeDiskMiB = 1024
	m.cfg.DiskFreeMiB = func(string) (int, error) { return free, nil }

	s, err := m.Create(ctx, CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Disk is low → refuse before touching the guest.
	free = 100
	if _, err := m.SleepInPlace(ctx, s.ID); !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("sleep under low disk err = %v, want ErrInsufficientDisk", err)
	}
	// A refused sleep must leave the instance running, not stuck half-slept: the
	// transition marker (sleeping) was never set — which is exactly what keeps it
	// routable (Routable returns false for a sleeping instance).
	m.mu.RLock()
	asleep, sleeping := s.asleep != nil, s.sleeping
	m.mu.RUnlock()
	if asleep || sleeping {
		t.Fatalf("refused sleep left bad state: asleep=%v sleeping=%v (want both false)", asleep, sleeping)
	}

	// Disk recovers → admit.
	free = 4096
	if _, err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep after disk recovered: %v", err)
	}
}

// TestSleepRegistersDurableSnapshotAndGCs covers the durability model: each
// sleep registers a DURABLE snapshot (so a slept app survives a daemon restart
// via re-adoption); an in-place wake KEEPS that snapshot (it backs the woken
// VM's lazy memory); the next sleep supersedes and GCs it; and Delete reclaims
// the backing snapshot. Exactly one snapshot is kept per instance.
func TestSleepRegistersDurableSnapshotAndGCs(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Sleep → a durable, registered snapshot with real files.
	if _, err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep 1: %v", err)
	}
	snap1 := s.memSnapshotID
	if snap1 == "" {
		t.Fatal("memSnapshotID not set after sleep")
	}
	if _, err := m.GetSnapshot(snap1); err != nil {
		t.Fatalf("sleep snapshot not registered: %v", err)
	}
	if _, err := os.Stat(s.asleep.statePath); err != nil {
		t.Fatalf("snapshot state file missing: %v", err)
	}

	// Wake KEEPS the snapshot — the woken VM's uffd pager reads its memory file.
	if err := m.WakeInPlace(ctx, s.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if s.memSnapshotID != snap1 {
		t.Fatalf("wake changed memSnapshotID to %q, want it kept at %q", s.memSnapshotID, snap1)
	}
	if _, err := m.GetSnapshot(snap1); err != nil {
		t.Fatalf("wake deleted the backing snapshot: %v", err)
	}

	// Second sleep → a fresh snapshot; the first is GC'd.
	if _, err := m.SleepInPlace(ctx, s.ID); err != nil {
		t.Fatalf("sleep 2: %v", err)
	}
	snap2 := s.memSnapshotID
	if snap2 == snap1 {
		t.Fatal("second sleep reused the snapshot id")
	}
	if _, err := m.GetSnapshot(snap2); err != nil {
		t.Fatalf("second snapshot missing: %v", err)
	}
	if _, err := m.GetSnapshot(snap1); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("previous snapshot not GC'd (err=%v)", err)
	}

	// Delete reclaims the backing snapshot (separate dir under WorkBase).
	if err := m.Delete(ctx, s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.GetSnapshot(snap2); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Delete did not GC the backing snapshot (err=%v)", err)
	}
}
