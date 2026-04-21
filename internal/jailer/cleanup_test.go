package jailer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupRemovesChroot(t *testing.T) {
	s := stageSpec(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	writeSrc(t, src, "x")
	if err := Stage(s, map[string]string{"/f": src}); err != nil {
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
	if err := Stage(s, map[string]string{"/f": src}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if err := Cleanup(s); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := Cleanup(s); err != nil {
		t.Fatalf("second Cleanup (should be no-op): %v", err)
	}
}
