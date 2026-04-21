package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Move relocates src to dst, preferring os.Rename and falling back to
// Clone + Remove on cross-device errors.
//
// The common case — src and dst on the same filesystem — collapses to
// a single directory entry update: no data is copied, the operation is
// O(1) in file size. When src and dst live on different filesystems
// (EXDEV), a byte-level copy is unavoidable; Clone uses FICLONE
// reflinks where the filesystem supports them and byte-copy otherwise,
// then Move unlinks src to preserve rename semantics.
//
// Move is the right primitive whenever a file has been written to a
// staging location (e.g., inside a jailer chroot) and needs to end up
// under its permanent name, without the caller having to know whether
// the two paths are colocated.
//
// dst is overwritten if it already exists; src is removed iff the copy
// succeeds (partial copies leave src in place so the caller can retry).
func Move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("fsutil: rename %s -> %s: %w", src, dst, err)
	}
	// Cross-device fallback: physical copy via Clone (FICLONE fast path,
	// byte copy fallback) then unlink src only on successful copy.
	if err := Clone(src, dst); err != nil {
		return fmt.Errorf("fsutil: cross-device move %s -> %s: %w", src, dst, err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("fsutil: remove src after cross-device copy: %w", err)
	}
	return nil
}
