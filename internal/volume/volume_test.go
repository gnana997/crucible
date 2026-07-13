package volume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestManager builds a Manager over a temp dir with a small default size
// (fast mkfs), chowning to the current user so no root is needed. Skips if
// mkfs.ext4 is unavailable.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), 8<<20 /* 8 MiB */, os.Getuid(), os.Getgid())
	if err != nil {
		if strings.Contains(err.Error(), "mkfs.ext4 not found") {
			t.Skip("mkfs.ext4 not available")
		}
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestAttachProvisionsOnceAndPersists(t *testing.T) {
	m := newTestManager(t)

	path, err := m.Attach("data", "sbx1")
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if filepath.Base(path) != "data.img" {
		t.Fatalf("backing path = %q, want .../data.img", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat backing file: %v", err)
	}
	firstMod := fi.ModTime()

	// Re-attach after release must NOT reformat (format-on-first-use only),
	// so the backing file is byte-identical — same mtime.
	m.Release("data")
	path2, err := m.Attach("data", "sbx2")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if path2 != path {
		t.Fatalf("re-attach path = %q, want %q", path2, path)
	}
	fi2, _ := os.Stat(path2)
	if !fi2.ModTime().Equal(firstMod) {
		t.Fatalf("backing file was reformatted on re-attach (mtime changed) — provision is not idempotent")
	}
}

func TestAttachGuardIsSingleWriter(t *testing.T) {
	m := newTestManager(t)

	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("Attach sbx1: %v", err)
	}
	// A second live sandbox must be refused — ext4 is single-writer.
	if _, err := m.Attach("v", "sbx2"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Attach sbx2 err = %v, want ErrInUse", err)
	}
	// Same sandbox re-attaching is fine (idempotent create path).
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("re-Attach sbx1: %v", err)
	}
	// After release, another sandbox can claim it.
	m.Release("v")
	if _, err := m.Attach("v", "sbx2"); err != nil {
		t.Fatalf("Attach sbx2 after release: %v", err)
	}
}

func TestAttachRejectsBadName(t *testing.T) {
	m := newTestManager(t)
	for _, bad := range []string{"", "UPPER", "has space", "../escape", "a/b"} {
		if _, err := m.Attach(bad, "sbx"); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Attach(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestNewManagerRequiresDir(t *testing.T) {
	if _, err := NewManager("", 0, 0, 0); err == nil {
		t.Fatal("NewManager(\"\") should error")
	}
}
