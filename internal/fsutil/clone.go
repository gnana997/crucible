// Package fsutil holds filesystem helpers crucible needs across layers.
//
// The single load-bearing function today is Clone — snapshot+fork
// requires copying multi-gigabyte rootfs and memory files per fork, and
// doing them via reflink (copy-on-write) is the difference between a
// sub-second fork and a multi-second one.
package fsutil

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Clone copies the file at src to dst, preferring an FICLONE ioctl
// (copy-on-write reflink) when the underlying filesystem supports it.
//
// On reflink-capable filesystems — XFS with reflink=1 (default since
// kernel 4.16), btrfs, modern f2fs — FICLONE creates dst as a lazy
// reference to src's extents. The call is O(1) in file size; writes
// to either file after that point diverge via COW. This is how sub-
// second fork is achieved for multi-GB memory + rootfs files.
//
// On filesystems without reflink support (ext4 by default, tmpfs, most
// /tmp mounts) the ioctl returns EOPNOTSUPP or EXDEV, and Clone falls
// back to a byte-level copy via io.Copy.
//
// dst is created if absent, truncated if present, with mode 0o640.
// Both src and dst must live on the same filesystem for FICLONE; if
// they don't, it errors and we fall back (FICLONE can't cross
// filesystems even when both individually support reflinks).
//
// Errors are wrapped with %w for errors.Is on os.IsNotExist etc.
func Clone(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("fsutil: open src %s: %w", src, err)
	}
	defer srcF.Close()

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("fsutil: create dst %s: %w", dst, err)
	}

	// Try FICLONE. On success the kernel has already made dst == src
	// via shared extents; no further IO is needed.
	if err := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd())); err == nil {
		return closeWithError(dstF, nil)
	}

	// Fallback path: full byte copy. The ioctl doesn't modify the
	// file on failure, so we can start writing at position 0 (we
	// opened with O_TRUNC).
	_, copyErr := io.Copy(dstF, srcF)
	return closeWithError(dstF, copyErr)
}

// closeWithError closes f and returns either the pre-existing err or a
// new one from Close (whichever is non-nil, preferring the original).
// Written out because `defer dstF.Close()` would hide Close errors on
// the fallback write path.
func closeWithError(f *os.File, err error) error {
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("fsutil: close %s: %w", f.Name(), closeErr)
	}
	return nil
}
