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
	"log/slog"
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
	defer func() { _ = srcF.Close() }()

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("fsutil: create dst %s: %w", dst, err)
	}

	// Try FICLONE. On success the kernel has already made dst == src
	// via shared extents; no further IO is needed.
	cloneErr := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd()))
	if cloneErr == nil {
		return closeWithError(dstF, nil)
	}

	// Fallback path: full byte copy. The ioctl doesn't modify the
	// file on failure, so we can start writing at position 0 (we
	// opened with O_TRUNC).
	slog.Default().Warn("reflink unavailable, falling back to full byte copy",
		"component", "fsutil", "src", src, "dst", dst, "err", cloneErr)
	_, copyErr := io.Copy(dstF, srcF)
	if copyErr == nil {
		// fsync before declaring success: reconcile can adopt this artifact
		// (snapshot rootfs/memory file) after a host crash, and an unsynced
		// copy may be zero-length or truncated after power loss.
		copyErr = dstF.Sync()
	}
	if copyErr != nil {
		// Don't leave a partial/corrupt dst behind — a retry should start
		// clean and reconcile must never adopt a half-written file.
		_ = dstF.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("fsutil: copy %s -> %s: %w", src, dst, copyErr)
	}
	return closeWithError(dstF, nil)
}

// CanReflink reports whether a Clone from a file in srcDir to a file in dstDir
// would use an O(1) FICLONE reflink (same filesystem + reflink support) rather
// than a full byte copy. It probes with tiny temp files and the real ioctl,
// cleaning them up. Used to gate work that is only worthwhile when reflink is
// available — e.g. a live volume backup, which freezes the guest only for the
// duration of the copy (acceptable when that copy is O(1), not a full byte copy).
func CanReflink(srcDir, dstDir string) bool {
	src, err := os.CreateTemp(srcDir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(src.Name()); _ = src.Close() }()
	if _, err := src.Write([]byte("probe")); err != nil {
		return false
	}
	dst, err := os.CreateTemp(dstDir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(dst.Name()); _ = dst.Close() }()
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
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
