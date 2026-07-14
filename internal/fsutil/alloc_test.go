package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllocatedBytesSparseAware(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 16 MiB logical, one 4 KiB page written: allocation must reflect the
	// written page (plus fs bookkeeping), not the logical size.
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := AllocatedBytes(path)
	if got <= 0 {
		t.Fatalf("AllocatedBytes = %d, want > 0", got)
	}
	if got >= 16<<20 {
		t.Fatalf("AllocatedBytes = %d, want far below the 16 MiB logical size (sparse)", got)
	}
}

func TestAllocatedBytesMissingIsZero(t *testing.T) {
	if got := AllocatedBytes(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("AllocatedBytes(missing) = %d, want 0", got)
	}
}

func TestDirAllocatedBytes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"state", "mem"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A subdirectory is skipped (snapshot artifact dirs are flat).
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}

	got := DirAllocatedBytes(dir)
	if got < 16384 {
		t.Fatalf("DirAllocatedBytes = %d, want >= 16384 (two 8 KiB files)", got)
	}
	if DirAllocatedBytes(filepath.Join(dir, "nope")) != 0 {
		t.Fatal("DirAllocatedBytes(missing dir) != 0")
	}
}
