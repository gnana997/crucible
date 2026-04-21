package jailer

import (
	"os"
	"path/filepath"
	"testing"
)

// stageSpec builds a Spec pointing at tmpdir for ChrootBase and at the
// current user for UID/GID (so os.Chown works without root in tests).
func stageSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		ID:         "test-vm",
		ExecFile:   "/usr/bin/firecracker",
		UID:        uint32(os.Getuid()),
		GID:        uint32(os.Getgid()),
		ChrootBase: t.TempDir(),
	}
}

func writeSrc(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write src %s: %v", path, err)
	}
}

func TestStageSingleFile(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "vmlinux")
	writeSrc(t, src, "kernel-bytes")

	if err := Stage(s, map[string]string{"/vmlinux": src}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	dst := HostPath(s, "/vmlinux")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read staged %s: %v", dst, err)
	}
	if string(got) != "kernel-bytes" {
		t.Fatalf("staged content = %q, want %q", got, "kernel-bytes")
	}
}

func TestStageMultipleFiles(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	kernel := filepath.Join(srcDir, "vmlinux")
	rootfs := filepath.Join(srcDir, "rootfs.ext4")
	writeSrc(t, kernel, "kernel")
	writeSrc(t, rootfs, "rootfs")

	err := Stage(s, map[string]string{
		"/vmlinux":     kernel,
		"/rootfs.ext4": rootfs,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	for chrootRel, want := range map[string]string{"/vmlinux": "kernel", "/rootfs.ext4": "rootfs"} {
		got, err := os.ReadFile(HostPath(s, chrootRel))
		if err != nil {
			t.Fatalf("read %s: %v", chrootRel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", chrootRel, got, want)
		}
	}
}

func TestStageCreatesNestedParents(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "snap.state")
	writeSrc(t, src, "state")

	if err := Stage(s, map[string]string{"/nested/deep/snap.state": src}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := os.Stat(HostPath(s, "/nested/deep/snap.state")); err != nil {
		t.Fatalf("nested parent not created: %v", err)
	}
}

func TestStageOverwritesExisting(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	srcA := filepath.Join(srcDir, "a")
	srcB := filepath.Join(srcDir, "b")
	writeSrc(t, srcA, "first")
	writeSrc(t, srcB, "second")

	if err := Stage(s, map[string]string{"/file": srcA}); err != nil {
		t.Fatalf("Stage first: %v", err)
	}
	if err := Stage(s, map[string]string{"/file": srcB}); err != nil {
		t.Fatalf("Stage second (overwrite): %v", err)
	}
	got, err := os.ReadFile(HostPath(s, "/file"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("after overwrite = %q, want %q", got, "second")
	}
}

func TestStageHardlinkSharesInode(t *testing.T) {
	// When src and dst live on the same filesystem (both under t.TempDir,
	// which they do), Stage must use os.Link — preserving inode. This is
	// the fast-path we rely on for snapshot restore (multiple forks share
	// one memory.file inode until they write, via COW at the firecracker
	// layer).
	s := stageSpec(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "mem.file")
	writeSrc(t, src, "mem")

	if err := Stage(s, map[string]string{"/mem.file": src}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	srcStat, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstStat, err := os.Stat(HostPath(s, "/mem.file"))
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !os.SameFile(srcStat, dstStat) {
		t.Fatal("expected hardlink (same inode) for same-filesystem Stage")
	}
}

func TestStageMissingSourceErrors(t *testing.T) {
	s := stageSpec(t)
	err := Stage(s, map[string]string{"/foo": "/nonexistent/path"})
	if err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
}
