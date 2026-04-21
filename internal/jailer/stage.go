package jailer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/fsutil"
)

// Stage places host files into this spec's chroot so firecracker can
// see them at chroot-relative paths after pivot_root.
//
// files maps chroot-relative destination paths → absolute host source
// paths. For each entry we:
//
//  1. Ensure the destination parent directory exists (0o750).
//  2. Try os.Link (hardlink). This is both fast and inode-sharing —
//     firecracker will read the same inode the daemon wrote, and a
//     single rootfs file can be staged into many forks without
//     multiplying disk usage.
//  3. On EXDEV (src and chroot live on different filesystems), fall
//     back to fsutil.Clone, which prefers FICLONE reflinks and falls
//     back to byte-copy.
//  4. chown the destination to spec.UID/GID so firecracker — running
//     as that user after jailer drops privileges — can open it.
//
// Stage fails fast on the first error. If a stage fails partway,
// the caller should invoke Cleanup(spec) to drop the whole chroot
// rather than trying to heal it in place.
//
// Re-staging the same destination overwrites it (stale dst is
// removed before os.Link; O_TRUNC handles the clone path).
func Stage(spec Spec, files map[string]string) error {
	root := ChrootRoot(spec)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("jailer: mkdir chroot root %s: %w", root, err)
	}

	for chrootRel, hostSrc := range files {
		dst := HostPath(spec, chrootRel)
		if err := stageOne(dst, hostSrc, spec.UID, spec.GID); err != nil {
			return err
		}
	}
	return nil
}

func stageOne(dst, src string, uid, gid uint32) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("jailer: mkdir parent of %s: %w", dst, err)
	}

	// Remove any stale dst so os.Link doesn't hit EEXIST. Missing
	// dst is fine; anything else is a real error worth surfacing.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("jailer: remove stale dst %s: %w", dst, err)
	}

	if err := os.Link(src, dst); err != nil {
		// Cross-filesystem hardlinks are impossible by kernel
		// design; fall back to a content-level copy. Anything else
		// (permission, missing src, etc.) is a real error.
		if !errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("jailer: hardlink %s -> %s: %w", src, dst, err)
		}
		if err := fsutil.Clone(src, dst); err != nil {
			return fmt.Errorf("jailer: clone fallback for %s: %w", src, err)
		}
	}

	if err := os.Chown(dst, int(uid), int(gid)); err != nil {
		return fmt.Errorf("jailer: chown %s to %d:%d: %w", dst, uid, gid, err)
	}
	return nil
}
