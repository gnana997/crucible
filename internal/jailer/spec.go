// Package jailer builds command lines for and manages the chroots
// owned by Firecracker's jailer binary.
//
// Jailer is Firecracker's production wrapper. Given a firecracker
// binary path and a VM id, it:
//
//  1. Creates a chroot at <ChrootBase>/firecracker/<ID>/root/ and
//     pivot_root's into it.
//  2. Opens fresh mount and (optionally) pid/network namespaces.
//  3. Creates a cgroup v2 cgroup under /sys/fs/cgroup/firecracker/<ID>
//     and writes the configured --cgroup key=value pairs into it.
//  4. mknods /dev/kvm and /dev/net/tun inside the chroot.
//  5. Drops to an unprivileged uid/gid.
//  6. Loads a seccomp-bpf filter.
//  7. Execs firecracker.
//
// Running every microVM under its own jailer chroot is how production
// Firecracker deployments (AWS Lambda, Fly.io) isolate VMs from each
// other at the filesystem and resource-control layers. It also fixes
// a subtle v1.15 bug for us: firecracker's vsock device records the
// host-side UDS path in snapshot state, and on load tries to re-bind
// it. Without jailer, forking a VM on the same host collides on that
// path. With jailer, every VM's "/v.sock" resolves to a different
// absolute host path (its own chroot), so the collision disappears
// as a byproduct of pivot_root.
//
// This package owns three concerns:
//
//  1. Building the jailer argv (argv.go). Pure function; Exec is the
//     caller's job — keeping Exec out of this package makes every
//     exported function side-effect free and trivially unit-testable.
//  2. Staging host files into the per-VM chroot so firecracker can
//     reference them by chroot-relative path after pivot_root
//     (stage.go). Hardlink-first, reflink/copy fallback.
//  3. Tearing down the chroot + cgroup on VM stop (cleanup.go).
//     Jailer itself does not clean up — upstream docs are explicit
//     that teardown is the wrapper's problem.
//
// See https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md
// for the upstream contract this package encodes.
package jailer

import (
	"fmt"
	"regexp"
)

// Spec declares a single jailed firecracker instance. It is pure
// data; pass the same Spec to BuildArgs, Stage, and Cleanup.
type Spec struct {
	// ID uniquely identifies this VM's chroot and cgroup. Must match
	// jailer's own validation rule ([a-zA-Z0-9-]{1,64}); we enforce
	// it in Validate so callers get a clear error instead of a
	// confusing jailer exit code.
	ID string

	// ExecFile is the absolute host path to the firecracker binary.
	// Jailer copies it into the chroot and execs it.
	ExecFile string

	// UID/GID is the unprivileged user jailer drops to before exec.
	// Any files Stage places into the chroot are chowned to this
	// uid/gid so post-drop firecracker can open them.
	UID uint32
	GID uint32

	// ChrootBase is the parent dir jailer creates VM chroots under.
	// Canonical value is /srv/jailer. Effective chroot root for this
	// VM is <ChrootBase>/firecracker/<ID>/root/.
	ChrootBase string

	// NewPIDNS puts firecracker in a fresh PID namespace so the VMM
	// sees itself as pid 1. Recommended: true. Costs nothing and
	// removes host pid visibility for anything inside the VM.
	NewPIDNS bool

	// NetNSPath, when non-empty, is passed to jailer as --netns.
	// Jailer joins that network namespace before execing
	// firecracker, so the VM's network-interfaces end up bound to
	// host-side devices inside that netns rather than the daemon's
	// root netns. Empty means jailer doesn't touch network
	// namespacing — firecracker inherits the daemon's netns.
	NetNSPath string

	// Quotas are enforced via cgroup v2 knobs written before exec.
	// Zero-valued fields are omitted (no limit on that dimension).
	Quotas Quotas
}

// Quotas is the subset of cgroup v2 controllers we plumb through.
// Zero means "omit that --cgroup flag" (unlimited).
type Quotas struct {
	// CPUMax is the cpu.max cgroup v2 entry in its native form:
	// "<quota> <period>" in microseconds. Example: "20000 100000"
	// caps CPU at 20% of one core. Empty = no CPU limit.
	CPUMax string

	// MemoryMaxBytes maps to memory.max (in bytes). When the cgroup
	// hits this, the guest gets OOM-killed inside its VM — the host
	// stays healthy. Zero = no memory limit.
	MemoryMaxBytes int64

	// PIDsMax maps to pids.max. This bounds processes in the VMM's
	// HOST cgroup (firecracker threads, jailer helper); it does not
	// bound guest processes, which live in a separate namespace the
	// host cgroup cannot see into. Useful as a cheap fork-bomb
	// protection for the VMM itself. Zero = no pids limit.
	PIDsMax int64
}

// validIDPattern mirrors jailer's own regex (per upstream jailer.md).
// We apply it up-front so invalid IDs surface as a clear Go error
// instead of a jailer fork-exec failure.
var validIDPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

// Validate checks invariants jailer would otherwise reject, plus a
// couple of our own (ExecFile and ChrootBase being non-empty). The
// returned error is safe to surface to end users.
func (s Spec) Validate() error {
	if !validIDPattern.MatchString(s.ID) {
		return fmt.Errorf("jailer: invalid ID %q (must match [a-zA-Z0-9-]{1,64})", s.ID)
	}
	if s.ExecFile == "" {
		return fmt.Errorf("jailer: ExecFile required")
	}
	if s.ChrootBase == "" {
		return fmt.Errorf("jailer: ChrootBase required")
	}
	return nil
}
