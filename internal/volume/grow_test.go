package volume

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// needResize2fs skips a test when resize2fs (e2fsprogs) is not on PATH.
func needResize2fs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("resize2fs"); err != nil {
		t.Skip("resize2fs not available")
	}
}

func TestGrowEnlargesBackingFileAndRecord(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	newSize := int64(testSize) * 3
	rec, err := m.Grow(context.Background(), "data", newSize)
	if err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if rec.SizeBytes != newSize {
		t.Fatalf("record size = %d, want %d", rec.SizeBytes, newSize)
	}
	// The record persisted.
	got, err := m.Get("data")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SizeBytes != newSize {
		t.Fatalf("persisted size = %d, want %d", got.SizeBytes, newSize)
	}
	// The backing file grew to the new logical size.
	fi, err := os.Stat(filepath.Join(m.dir, "data.img"))
	if err != nil {
		t.Fatalf("stat backing: %v", err)
	}
	if fi.Size() != newSize {
		t.Fatalf("backing file size = %d, want %d", fi.Size(), newSize)
	}
	// The filesystem is still usable after the resize — a fresh attach succeeds.
	if _, _, err := m.Attach("data", "sbx1"); err != nil {
		t.Fatalf("Attach after grow: %v", err)
	}
	m.Release("data")
}

func TestGrowRefusesShrinkOrSame(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Grow(context.Background(), "data", testSize); !errors.Is(err, ErrNotLarger) {
		t.Fatalf("Grow to same size err = %v, want ErrNotLarger", err)
	}
	if _, err := m.Grow(context.Background(), "data", testSize/2); !errors.Is(err, ErrNotLarger) {
		t.Fatalf("Grow to smaller err = %v, want ErrNotLarger", err)
	}
}

func TestGrowRefusesAttachedVolume(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := m.Attach("data", "sbx1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer m.Release("data")
	if _, err := m.Grow(context.Background(), "data", int64(testSize)*2); !errors.Is(err, ErrInUse) {
		t.Fatalf("Grow while attached err = %v, want ErrInUse", err)
	}
}

func TestGrowMissingVolume(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Grow(context.Background(), "nope", int64(testSize)*2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Grow missing err = %v, want ErrNotFound", err)
	}
}
