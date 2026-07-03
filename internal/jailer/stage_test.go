package jailer

import (
	"os"
	"path/filepath"
	"syscall"
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

	if err := Stage(s, map[string]StageFile{"/vmlinux": {Src: src}}); err != nil {
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

	err := Stage(s, map[string]StageFile{
		"/vmlinux":     {Src: kernel},
		"/rootfs.ext4": {Src: rootfs},
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

	if err := Stage(s, map[string]StageFile{"/nested/deep/snap.state": {Src: src}}); err != nil {
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

	if err := Stage(s, map[string]StageFile{"/file": {Src: srcA}}); err != nil {
		t.Fatalf("Stage first: %v", err)
	}
	if err := Stage(s, map[string]StageFile{"/file": {Src: srcB}}); err != nil {
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

	if err := Stage(s, map[string]StageFile{"/mem.file": {Src: src}}); err != nil {
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

// TestStageSharedFileIsolatesInode is the H1 regression: a Shared
// source (the daemon-wide kernel) must be staged as its OWN inode,
// read-only, and must NOT have its ownership or contents mutated by
// staging — so the post-stage chown can never downgrade the shared
// original and a compromised VMM can't rewrite it for future tenants.
func TestStageSharedFileIsolatesInode(t *testing.T) {
	s := stageSpec(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "vmlinux")
	writeSrc(t, src, "kernel-bytes")
	// Mark the shared source owner-writable (0644) so a regression to
	// the old hardlink+chown path would visibly share this inode.
	if err := os.Chmod(src, 0o644); err != nil {
		t.Fatalf("chmod src: %v", err)
	}
	srcStatBefore, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}

	if err := Stage(s, map[string]StageFile{"/vmlinux": {Src: src, Shared: true}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	dst := HostPath(s, "/vmlinux")
	dstStat, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	// Separate inode — not a hardlink to the shared source.
	if os.SameFile(srcStatBefore, dstStat) {
		t.Fatal("shared file was hardlinked (shares inode with source) — chown would poison the shared kernel")
	}
	// Staged copy is read-only.
	if perm := dstStat.Mode().Perm(); perm != 0o444 {
		t.Errorf("staged shared file mode = %o, want 0444", perm)
	}
	// The shared SOURCE is untouched: same mode it had before staging.
	srcStatAfter, err := os.Stat(src)
	if err != nil {
		t.Fatalf("re-stat src: %v", err)
	}
	if perm := srcStatAfter.Mode().Perm(); perm != 0o644 {
		t.Errorf("shared source mode changed to %o, want 0644 (staging must not mutate the shared inode)", perm)
	}
	// Content still readable and intact.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read staged shared file: %v", err)
	}
	if string(got) != "kernel-bytes" {
		t.Errorf("staged shared content = %q, want %q", got, "kernel-bytes")
	}
}

// TestStageSharedKernelPreservesOwnershipRoot is the privileged H1
// end-to-end check: staging the shared kernel as an unprivileged jail
// uid (10000, the value production uses) must NOT change the shared
// source's owner, while a private file staged the same run IS chowned
// to that uid. Only root can chown to a foreign uid, so this proves
// the real cross-tenant scenario the unit test can only approximate.
//
// Run: sudo CRUCIBLE_ROOT_TEST=1 go test ./internal/jailer \
//        -run TestStageSharedKernelPreservesOwnershipRoot -v
func TestStageSharedKernelPreservesOwnershipRoot(t *testing.T) {
	if os.Getenv("CRUCIBLE_ROOT_TEST") == "" || os.Geteuid() != 0 {
		t.Skip("privileged test; set CRUCIBLE_ROOT_TEST=1 and run as root")
	}
	const jailUID, jailGID = 10000, 10000

	s := Spec{
		ID:         "test-vm",
		ExecFile:   "/usr/bin/firecracker",
		UID:        jailUID,
		GID:        jailGID,
		ChrootBase: t.TempDir(),
	}
	srcDir := t.TempDir()
	kernel := filepath.Join(srcDir, "vmlinux") // shared, owner-writable
	rootfs := filepath.Join(srcDir, "rootfs.ext4")
	writeSrc(t, kernel, "kernel")
	writeSrc(t, rootfs, "rootfs")
	if err := os.Chmod(kernel, 0o644); err != nil {
		t.Fatalf("chmod kernel: %v", err)
	}

	// Exactly the map JailerRunner.Start builds.
	if err := Stage(s, map[string]StageFile{
		"/vmlinux":     {Src: kernel, Shared: true},
		"/rootfs.ext4": {Src: rootfs},
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// The SHARED kernel source keeps root ownership + its 0644 mode.
	var kst syscall.Stat_t
	if err := syscall.Stat(kernel, &kst); err != nil {
		t.Fatalf("stat kernel: %v", err)
	}
	if kst.Uid != 0 || kst.Gid != 0 {
		t.Errorf("shared kernel source downgraded to %d:%d — a compromised VMM could now poison it", kst.Uid, kst.Gid)
	}
	if kst.Nlink != 1 {
		t.Errorf("shared kernel source has nlink=%d, want 1 (staging must not hardlink it)", kst.Nlink)
	}

	// The PRIVATE rootfs clone WAS chowned to the jail uid, as intended.
	var rst syscall.Stat_t
	if err := syscall.Stat(HostPath(s, "/rootfs.ext4"), &rst); err != nil {
		t.Fatalf("stat staged rootfs: %v", err)
	}
	if rst.Uid != jailUID || rst.Gid != jailGID {
		t.Errorf("staged private rootfs owner = %d:%d, want %d:%d", rst.Uid, rst.Gid, jailUID, jailGID)
	}
}

func TestStageMissingSourceErrors(t *testing.T) {
	s := stageSpec(t)
	err := Stage(s, map[string]StageFile{"/foo": {Src: "/nonexistent/path"}})
	if err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
}
