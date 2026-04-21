package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := Move(src, dst); err != nil {
		t.Fatalf("Move: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("dst content = %q, want %q", got, "hello")
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src still exists after Move: err=%v", err)
	}
}

func TestMoveOverwritesDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	_ = os.WriteFile(src, []byte("new"), 0o640)
	_ = os.WriteFile(dst, []byte("stale"), 0o640)

	if err := Move(src, dst); err != nil {
		t.Fatalf("Move: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "new" {
		t.Errorf("dst = %q, want overwritten with %q", got, "new")
	}
}

func TestMoveMissingSource(t *testing.T) {
	dir := t.TempDir()
	err := Move(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}
