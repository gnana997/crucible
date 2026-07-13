package volume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSize = 8 << 20 // 8 MiB — small + fast mkfs

// newMgr builds a Manager over dir, chowning to the current user (no root
// needed). Skips if mkfs.ext4 is unavailable.
func newMgr(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		if strings.Contains(err.Error(), "mkfs.ext4 not found") {
			t.Skip("mkfs.ext4 not available")
		}
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestAttachProvisionsOnceAndPersists(t *testing.T) {
	m := newMgr(t, t.TempDir())

	path, err := m.Attach("data", "sbx1")
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if filepath.Base(path) != "data.img" {
		t.Fatalf("backing path = %q, want .../data.img", path)
	}
	fi, _ := os.Stat(path)
	firstMod := fi.ModTime()

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
		t.Fatalf("backing file reformatted on re-attach — provision not idempotent")
	}
}

func TestAttachGuardIsSingleWriter(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("Attach sbx1: %v", err)
	}
	if _, err := m.Attach("v", "sbx2"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Attach sbx2 err = %v, want ErrInUse", err)
	}
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("re-Attach sbx1: %v", err)
	}
	m.Release("v")
	if _, err := m.Attach("v", "sbx2"); err != nil {
		t.Fatalf("Attach sbx2 after release: %v", err)
	}
}

func TestAttachRejectsBadName(t *testing.T) {
	m := newMgr(t, t.TempDir())
	for _, bad := range []string{"", "UPPER", "has space", "../escape", "a/b"} {
		if _, err := m.Attach(bad, "sbx"); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Attach(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestCreateListAndDuplicate(t *testing.T) {
	m := newMgr(t, t.TempDir())
	rec, err := m.Create("big", 16<<20)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.SizeBytes != 16<<20 || rec.HostID != "testhost" || !rec.Formatted {
		t.Fatalf("record = %+v, want size 16MiB, host testhost, formatted", rec)
	}
	infos, err := m.List()
	if err != nil || len(infos) != 1 || infos[0].Name != "big" {
		t.Fatalf("List = %+v, err %v; want one volume 'big'", infos, err)
	}
	if _, err := m.Create("big", 0); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
}

func TestRemoveRefusesAttachedThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := m.Remove("v"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Remove while attached err = %v, want ErrInUse", err)
	}
	m.Release("v")
	if err := m.Remove("v"); err != nil {
		t.Fatalf("Remove after release: %v", err)
	}
	if _, err := m.Get("v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v.img")); !os.IsNotExist(err) {
		t.Fatalf("backing file still present after Remove")
	}
	if err := m.Remove("v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove nonexistent err = %v, want ErrNotFound", err)
	}
}

func TestRecordsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	m1 := newMgr(t, dir)
	if _, err := m1.Create("keep", 16<<20); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = m1.Close()

	m2, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()
	got, err := m2.Get("keep")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.SizeBytes != 16<<20 {
		t.Fatalf("reopened size = %d, want 16MiB (record not durable)", got.SizeBytes)
	}
}

func TestBackfillAdoptsBareImg(t *testing.T) {
	dir := t.TempDir()
	// A bare backing file with no record (simulates a V-M1 volume).
	if err := os.WriteFile(filepath.Join(dir, "legacy.img"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write bare img: %v", err)
	}
	m := newMgr(t, dir)
	got, err := m.Get("legacy")
	if err != nil {
		t.Fatalf("backfilled volume not found: %v", err)
	}
	if got.Name != "legacy" {
		t.Fatalf("backfill name = %q", got.Name)
	}
}

func TestNewManagerRequiresDir(t *testing.T) {
	if _, err := NewManager("", 0, "", 0, 0); err == nil {
		t.Fatal("NewManager(\"\") should error")
	}
}
