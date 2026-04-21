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

// ReapOrphans removes every per-VM chroot + matching cgroup under
// the given ChrootBase that was left behind by a previous daemon
// invocation.
//
// The daemon calls this exactly once, at startup, before accepting
// any requests. Sandboxes are ephemeral and in-memory only — no
// state carries across a daemon restart — so every directory found
// under <ChrootBase>/<basename(execFile)>/ at startup is by
// definition an orphan from a prior run that crashed or was killed
// without clean shutdown.
//
// execFile is the same absolute path the daemon passes to
// JailerRunner; only its basename matters here (it names the
// subdirectory layer jailer itself writes under).
//
// Returns the list of IDs that were reaped (for logging), plus the
// first error encountered, if any. Missing ChrootBase is not an
// error — the first ever daemon startup has nothing to reap.
func ReapOrphans(chrootBase, execFile string) ([]string, error) {
	parent := filepath.Join(chrootBase, filepath.Base(execFile))
	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("jailer: scan %s for orphans: %w", parent, err)
	}

	reaped := make([]string, 0, len(entries))
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		spec := Spec{ID: e.Name(), ExecFile: execFile, ChrootBase: chrootBase}
		if err := Cleanup(spec); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("jailer: reap %s: %w", e.Name(), err)
			continue
		}
		reaped = append(reaped, e.Name())
	}
	return reaped, firstErr
}
