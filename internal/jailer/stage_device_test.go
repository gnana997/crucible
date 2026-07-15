package jailer

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A Device stage of a regular (non-block) file must be refused outright — a
// safety guard so a mistaken caller can never mknod something unexpected into a
// jail. No root needed: the rejection happens before any mknod.
func TestStageDeviceRejectsNonBlock(t *testing.T) {
	s := stageSpec(t)
	src := filepath.Join(t.TempDir(), "not-a-device")
	writeSrc(t, src, "regular file")

	err := Stage(s, map[string]StageFile{"/vol0.ext4": {Src: src, Device: true}})
	if err == nil {
		t.Fatal("staging a regular file as a Device must fail")
	}
	if _, statErr := os.Stat(HostPath(s, "/vol0.ext4")); statErr == nil {
		t.Fatal("no node should have been created for a rejected device stage")
	}
}

// With root, a Device stage mknod's a block-special node into the chroot with the
// source device's major:minor, owned by the jail uid, 0600.
func TestStageDeviceMknodMatchesRdev(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (mknod)")
	}
	// /dev/loop0 need not exist; use /dev/ram0 if present, else any block device.
	src := firstBlockDevice(t)

	var srcSt unix.Stat_t
	if err := unix.Stat(src, &srcSt); err != nil {
		t.Fatalf("stat %s: %v", src, err)
	}

	s := stageSpec(t)
	if err := Stage(s, map[string]StageFile{"/vol0.ext4": {Src: src, Device: true}}); err != nil {
		t.Fatalf("Stage device: %v", err)
	}

	dst := HostPath(s, "/vol0.ext4")
	var dstSt unix.Stat_t
	if err := unix.Stat(dst, &dstSt); err != nil {
		t.Fatalf("stat staged node: %v", err)
	}
	if dstSt.Mode&unix.S_IFMT != unix.S_IFBLK {
		t.Fatalf("staged node is not a block device: mode %#o", dstSt.Mode)
	}
	if dstSt.Rdev != srcSt.Rdev {
		t.Fatalf("staged rdev %d != source rdev %d", dstSt.Rdev, srcSt.Rdev)
	}
	if dstSt.Mode&0o777 != 0o600 {
		t.Fatalf("staged node mode = %#o, want 0600", dstSt.Mode&0o777)
	}
	if int(dstSt.Uid) != os.Getuid() {
		t.Fatalf("staged node uid = %d, want %d", dstSt.Uid, os.Getuid())
	}
}

func firstBlockDevice(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{"/dev/ram0", "/dev/loop0", "/dev/sda", "/dev/vda", "/dev/nvme0n1"} {
		var st unix.Stat_t
		if err := unix.Stat(cand, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK {
			return cand
		}
	}
	t.Skip("no block device available to stage")
	return ""
}
