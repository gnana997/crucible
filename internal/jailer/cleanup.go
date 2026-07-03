package jailer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
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

// reapKillTimeout bounds how long killJailed waits for a SIGKILL'd VMM
// to actually exit (releasing its chroot bind mounts) before Cleanup
// removes the tree.
const reapKillTimeout = 2 * time.Second

// jailedPIDs returns the host PIDs of the jailer + firecracker processes
// belonging to the VM with the given jailer ID.
//
// We match on /proc/<pid>/cmdline, not /proc/<pid>/root: jailer
// pivot_root's firecracker into a PRIVATE mount namespace, so from the
// host the process's root is unreachable and /proc/<pid>/root does not
// resolve to <ChrootBase>/firecracker/<ID>/root. cmdline, by contrast,
// is plain host-readable bytes unaffected by the mount/pid namespace.
//
// The match requires the ID to appear as the token immediately after a
// literal "--id" — the exact shape jailer emits into both its own argv
// and firecracker's (see BuildArgs). Matching a bare token anywhere in
// the argv would let an ambiguous ID (jailer's own regex permits "1")
// select unrelated host processes such as `sleep 1` — a stray SIGKILL
// vector the reap must not have.
func jailedPIDs(id string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID entry
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // process exited, or unreadable — skip
		}
		if cmdlineMatchesID(raw, id) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// cmdlineMatchesID reports whether raw — a NUL-separated
// /proc/<pid>/cmdline — carries the jailer "--id <id>" argument pair.
// Requiring id to directly follow a "--id" token (rather than matching
// a bare token anywhere) is what keeps an ambiguous id like "1" from
// selecting unrelated processes. Pure + allocation-light so it's unit-
// testable without fabricating /proc entries.
func cmdlineMatchesID(raw []byte, id string) bool {
	idTok := []byte(id)
	dashID := []byte("--id")
	toks := bytes.Split(raw, []byte{0})
	for i := 1; i < len(toks); i++ {
		if bytes.Equal(toks[i-1], dashID) && bytes.Equal(toks[i], idTok) {
			return true
		}
	}
	return false
}

// killJailed SIGKILLs the jailer + firecracker processes for the VM with
// the given jailer ID and waits, bounded, for them to exit so the
// chroot's bind mounts are released before the caller removes the tree.
//
// Returns whether the process set — as identified by /proc/<pid>/
// cmdline — drained within reapKillTimeout. A false return means at
// least one matching process is still present after the deadline; the
// caller MUST NOT remove that chroot, since the VM is still live and
// holding its bind mounts. The classic false case is a task wedged in
// uninterruptible D-state, which SIGKILL cannot reap until its kernel
// operation completes. A cleanly-shut-down VM has no matching process,
// so this returns true immediately (the common case).
//
// Liveness here trusts the "--id <ID>" tokens in cmdline. That is
// robust against the non-adversarial cases this reap targets (crash/
// SIGKILL orphans, slow or D-state exits), but a fully compromised VMM
// can rewrite its own argv to erase the token and thereby look
// drained. We accept that: sandbox IDs are random and never reused, so
// a later VM won't collide on the freed slot, and RemoveAll of a live
// VMM's tree stalls on EBUSY at its bind mounts (partial unlink; the
// VMM keeps running on its open fds) rather than silently killing it.
// The daemon runs jailer without --cgroup by default, so the
// cgroup.procs signal that would be spoof-proof usually doesn't exist.
func killJailed(id string) bool {
	pids := jailedPIDs(id)
	if len(pids) == 0 {
		return true
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(reapKillTimeout)
	for {
		if len(jailedPIDs(id)) == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
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
		id := e.Name()
		// The reap path takes this directory name straight to
		// killJailed, which SIGKILLs every host process whose argv
		// carries the token. A name that isn't a valid jailer ID is
		// not one we created (the create path enforces validIDPattern
		// via Spec.Validate) — refuse to use it as a kill selector,
		// or a stray dir named e.g. "1" would SIGKILL every process
		// with a bare "1" argv token.
		if !validIDPattern.MatchString(id) {
			if firstErr == nil {
				firstErr = fmt.Errorf("jailer: refusing to reap unexpected dir name %q under %s", id, parent)
			}
			continue
		}
		spec := Spec{ID: id, ExecFile: execFile, ChrootBase: chrootBase}
		// A VM whose daemon was killed without clean shutdown keeps
		// running (reparented to init) — the chroot directory alone is
		// not enough to identify it as dead. Kill the jailer +
		// firecracker carrying this VM's ID before removing the tree,
		// so a restart leaves no orphaned VM process behind.
		if !killJailed(id) {
			// The VM is still live after the kill timeout (e.g. a
			// wedged D-state task). Removing its chroot now would
			// unlink the tree out from under a running VM whose bind
			// mounts we never released — an orphan leak, not a reap.
			// Leave the tree in place and surface the error so the
			// next startup retries.
			if firstErr == nil {
				firstErr = fmt.Errorf("jailer: VM %s still live after kill timeout; left chroot in place", id)
			}
			continue
		}
		if err := Cleanup(spec); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("jailer: reap %s: %w", id, err)
			continue
		}
		reaped = append(reaped, id)
	}
	return reaped, firstErr
}
