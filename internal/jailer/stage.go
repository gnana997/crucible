package jailer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/fsutil"
)

// StageFile describes one host file to place into a chroot.
type StageFile struct {
	// Src is the absolute host path to link or copy from.
	Src string

	// Shared marks Src as an inode shared across VMs — the canonical
	// case is the daemon-wide kernel, one file every cold-boot
	// sandbox stages. Shared sources are staged as an independent,
	// read-only (0o444) copy, never a hardlink, and are NOT chowned:
	//
	//   - a hardlink shares Src's inode, so the post-stage chown
	//     would rewrite the *shared* file's owner to the jail uid,
	//     and if its mode granted owner-write a compromised VMM
	//     could then rewrite the kernel every future tenant boots.
	//   - an independent 0o444 copy is world-readable (so firecracker
	//     as the jail uid can still open it) but unwritable, and lives
	//     on its own inode, so nothing the VMM does reaches the
	//     original.
	//
	// Private per-VM sources (the per-sandbox rootfs clone, snapshot
	// artifacts) leave this false: they hardlink-then-chown as before,
	// which is safe precisely because the inode is this VM's alone.
	Shared bool
}

// Stage places host files into this spec's chroot so firecracker can
// see them at chroot-relative paths after pivot_root.
//
// files maps chroot-relative destination paths → StageFile. For a
// private (Shared == false) entry we:
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
// A Shared entry instead always copies to an independent inode and
// stages it 0o444 (see StageFile.Shared) — no hardlink, no chown.
//
// Stage fails fast on the first error. If a stage fails partway,
// the caller should invoke Cleanup(spec) to drop the whole chroot
// rather than trying to heal it in place.
//
// Re-staging the same destination overwrites it (stale dst is
// removed before os.Link; O_TRUNC handles the clone path).
func Stage(spec Spec, files map[string]StageFile) error {
	root := ChrootRoot(spec)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("jailer: mkdir chroot root %s: %w", root, err)
	}

	for chrootRel, f := range files {
		dst := HostPath(spec, chrootRel)
		if err := stageOne(dst, f, spec.UID, spec.GID); err != nil {
			return err
		}
	}
	return nil
}

func stageOne(dst string, f StageFile, uid, gid uint32) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("jailer: mkdir parent of %s: %w", dst, err)
	}

	// Remove any stale dst so os.Link doesn't hit EEXIST. Missing
	// dst is fine; anything else is a real error worth surfacing.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("jailer: remove stale dst %s: %w", dst, err)
	}

	if f.Shared {
		// Independent, read-only copy — own inode, so neither the
		// chmod below nor any VMM write can reach the shared source.
		// fsutil.Clone prefers a FICLONE reflink (COW: a new inode
		// sharing extents, diverging on write) and falls back to a
		// byte copy; both give dst its own inode.
		if err := fsutil.Clone(f.Src, dst); err != nil {
			return fmt.Errorf("jailer: copy shared file %s -> %s: %w", f.Src, dst, err)
		}
		// Clone creates dst 0o640 owned by the daemon; firecracker as
		// the jail uid can't read that. 0o444 makes it world-readable
		// but unwritable. The security property that matters is inode
		// isolation: this is a private per-VM copy, so even if a
		// compromised VMM later flips its own copy writable and
		// rewrites it, nothing reaches the shared kernel every other
		// tenant boots from.
		if err := os.Chmod(dst, 0o444); err != nil {
			return fmt.Errorf("jailer: chmod %s 0444: %w", dst, err)
		}
		return nil
	}

	if err := os.Link(f.Src, dst); err != nil {
		// Cross-filesystem hardlinks are impossible by kernel
		// design; fall back to a content-level copy. Anything else
		// (permission, missing src, etc.) is a real error.
		if !errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("jailer: hardlink %s -> %s: %w", f.Src, dst, err)
		}
		if err := fsutil.Clone(f.Src, dst); err != nil {
			return fmt.Errorf("jailer: clone fallback for %s: %w", f.Src, err)
		}
	}

	if err := os.Chown(dst, int(uid), int(gid)); err != nil {
		return fmt.Errorf("jailer: chown %s to %d:%d: %w", dst, uid, gid, err)
	}
	return nil
}
