package jailer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Cleanup tears down the chroot and cgroup owned by this Spec.
//
// Two pieces of state outlive the jailer process and must be removed
// by us:
//
//  1. The chroot dir tree at <ChrootBase>/firecracker/<ID>/ (the
//     parent of ChrootRoot, including it). Upstream jailer docs are
//     explicit: "It's up to the user to handle cleanup after running
//     the jailer."
//  2. The cgroup at /sys/fs/cgroup/firecracker/<ID>. Jailer only
//     creates this when at least one --cgroup flag was passed. An
//     rmdir on a non-existent cgroup returns ENOENT (fine); an rmdir
//     on a non-empty cgroup returns EBUSY (a real bug — some
//     process is still alive in it).
//
// Both operations are best-effort: we collect the first error and
// keep going so a partial failure doesn't block the daemon from
// reaping other VMs. ENOENT is not treated as an error in either
// path (Cleanup is idempotent).
func Cleanup(spec Spec) error {
	vmDir := ChrootDir(spec)
	cgroupDir := filepath.Join("/sys/fs/cgroup", filepath.Base(spec.ExecFile), spec.ID)

	var firstErr error

	if err := os.RemoveAll(vmDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		firstErr = fmt.Errorf("jailer: remove chroot %s: %w", vmDir, err)
	}

	// os.Remove (not RemoveAll) on cgroupDir: cgroupfs rmdir is the
	// actual kernel destruction call. RemoveAll would try to unlink
	// files inside the cgroup dir first, which is wrong — cgroup
	// controller files aren't regular files we should delete.
	if err := os.Remove(cgroupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if firstErr == nil {
			firstErr = fmt.Errorf("jailer: remove cgroup %s: %w", cgroupDir, err)
		}
	}

	return firstErr
}
