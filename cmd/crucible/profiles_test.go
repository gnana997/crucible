package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProfiles(t *testing.T) {
	dir := t.TempDir()
	// Two real images, a non-image file, and a subdir (all ignored except
	// the .ext4 files).
	writeEmpty(t, filepath.Join(dir, "base.ext4"))
	writeEmpty(t, filepath.Join(dir, "python-3.12.ext4"))
	writeEmpty(t, filepath.Join(dir, "README.md"))
	if err := os.Mkdir(filepath.Join(dir, "sub.ext4"), 0o750); err != nil {
		t.Fatal(err)
	}
	// An alias symlink: node.ext4 -> node-22.ext4.
	writeEmpty(t, filepath.Join(dir, "node-22.ext4"))
	if err := os.Symlink("node-22.ext4", filepath.Join(dir, "node.ext4")); err != nil {
		t.Fatal(err)
	}

	got, err := discoverProfiles(dir)
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}

	want := []string{"base", "python-3.12", "node-22", "node"}
	if len(got) != len(want) {
		t.Fatalf("discovered %d profiles %v, want %d %v", len(got), keys(got), len(want), want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing profile %q (got %v)", name, keys(got))
		}
	}
	// The alias resolves to the real image path.
	if got["node"] != got["node-22"] {
		t.Errorf("alias node = %q, want same as node-22 = %q", got["node"], got["node-22"])
	}
}

func TestDiscoverProfilesBrokenSymlinkErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("missing.ext4", filepath.Join(dir, "ghost.ext4")); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverProfiles(dir); err == nil {
		t.Fatal("discoverProfiles: want error on broken symlink, got nil")
	}
}

func writeEmpty(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
