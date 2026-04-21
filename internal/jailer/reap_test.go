package jailer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// seedOrphan creates a minimal chroot layout mimicking one a previous
// daemon run would have left behind. Used by reap tests.
func seedOrphan(t *testing.T, base, execBasename, id string) {
	t.Helper()
	chroot := filepath.Join(base, execBasename, id, "root")
	if err := os.MkdirAll(chroot, 0o750); err != nil {
		t.Fatalf("seed chroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chroot, "stale.txt"), []byte("x"), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}
}

func TestReapOrphansRemovesStaleChroots(t *testing.T) {
	base := t.TempDir()
	execFile := "/usr/bin/firecracker"

	for _, id := range []string{"sbx-a", "sbx-b", "sbx-c"} {
		seedOrphan(t, base, "firecracker", id)
	}

	reaped, err := ReapOrphans(base, execFile)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	slices.Sort(reaped)
	want := []string{"sbx-a", "sbx-b", "sbx-c"}
	if !slices.Equal(reaped, want) {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}

	// The parent dir should still exist (we only removed the children)
	// but it should be empty.
	parent := filepath.Join(base, "firecracker")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("parent not empty: %v", entries)
	}
}

func TestReapOrphansNoDirIsNotError(t *testing.T) {
	// First-ever daemon startup: ChrootBase exists but has never had a
	// jailer subdirectory under it.
	reaped, err := ReapOrphans(t.TempDir(), "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped on empty dir: %v", reaped)
	}
}

func TestReapOrphansSkipsNonDirEntries(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "firecracker")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A stray regular file under <base>/firecracker/ shouldn't make
	// ReapOrphans choke (jailer wouldn't create such a thing, but we
	// shouldn't crash on it either).
	if err := os.WriteFile(filepath.Join(parent, "stray.txt"), []byte("junk"), 0o640); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	seedOrphan(t, base, "firecracker", "sbx-real")

	reaped, err := ReapOrphans(base, "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "sbx-real" {
		t.Fatalf("reaped = %v, want [sbx-real]", reaped)
	}
}
