package jailer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCleanupRemovesChroot(t *testing.T) {
	s := stageSpec(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	writeSrc(t, src, "x")
	if err := Stage(s, map[string]StageFile{"/f": {Src: src}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := os.Stat(ChrootRoot(s)); err != nil {
		t.Fatalf("chroot should exist pre-cleanup: %v", err)
	}

	if err := Cleanup(s); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(ChrootDir(s)); !os.IsNotExist(err) {
		t.Fatalf("chroot dir should be gone after Cleanup, stat err = %v", err)
	}
}

func TestCleanupIdempotent(t *testing.T) {
	s := stageSpec(t)
	// Never stage anything; Cleanup on a pristine spec must still be OK.
	if err := Cleanup(s); err != nil {
		t.Fatalf("first Cleanup on empty spec: %v", err)
	}
	if err := Cleanup(s); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}

func TestCleanupAfterStageThenDoubleCleanup(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	writeSrc(t, src, "x")
	if err := Stage(s, map[string]StageFile{"/f": {Src: src}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if err := Cleanup(s); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := Cleanup(s); err != nil {
		t.Fatalf("second Cleanup (should be no-op): %v", err)
	}
}

func TestRemoveWithRetryTransientBusy(t *testing.T) {
	// EBUSY on the first two attempts, then success — the drain-window case.
	calls := 0
	err := removeWithRetry(func() error {
		calls++
		if calls < 3 {
			return &os.PathError{Op: "remove", Path: "cg", Err: syscall.EBUSY}
		}
		return nil
	}, 100*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("want nil after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRemoveWithRetryPersistentBusy(t *testing.T) {
	// Never drains — a genuine leak surfaces as EBUSY after the budget.
	calls := 0
	err := removeWithRetry(func() error {
		calls++
		return &os.PathError{Op: "remove", Path: "cg", Err: syscall.EBUSY}
	}, 5*time.Millisecond, time.Millisecond)
	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("want EBUSY after budget, got %v", err)
	}
	if calls < 2 {
		t.Errorf("calls = %d, want at least 2 retries within the budget", calls)
	}
}

func TestRemoveWithRetryNonBusyIsImmediate(t *testing.T) {
	// A non-EBUSY error returns at once — no retry, no sleep.
	sentinel := errors.New("boom")
	calls := 0
	err := removeWithRetry(func() error { calls++; return sentinel }, time.Second, time.Millisecond)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-EBUSY)", calls)
	}
}

func TestRemoveWithRetryNotExistIsSuccess(t *testing.T) {
	if err := removeWithRetry(func() error { return os.ErrNotExist }, time.Second, time.Millisecond); err != nil {
		t.Fatalf("ENOENT should be treated as success, got %v", err)
	}
}
