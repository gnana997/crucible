// Package runner launches and supervises Firecracker microVM processes.
//
// Firecracker is one-VM-per-process by design, so every Runner.Start call
// spawns a new `firecracker` binary. A Start does the full boot sequence:
//
//  1. Creates Spec.Workdir and opens a log file inside it.
//  2. Spawns `firecracker --api-sock <workdir>/api.sock`, redirecting its
//     stdout and stderr (including the guest serial console) to the log.
//  3. Polls the API socket until GET / responds — firecracker takes a few
//     milliseconds to finish initializing before it listens.
//  4. Configures the VM via fcapi: PUT /boot-source, PUT /drives/rootfs,
//     PUT /machine-config.
//  5. Calls PUT /actions { InstanceStart } to power on the VM.
//  6. Returns a Handle the caller uses to shut down or wait.
//
// This package is deliberately narrow: no snapshot/fork, no network, no
// vsock, no jailer. Those features land in later weeks and will compose
// with Spec by adding fields (or sub-specs), not by forking this type.
package runner

import (
	"context"
	"errors"
)

// DefaultBootArgs is the Linux kernel command line applied when Spec.BootArgs
// is empty. Chosen for Firecracker's device model:
//
//   - console=ttyS0   guest serial console goes to the VMM process stdout,
//     which the runner redirects to <workdir>/firecracker.log.
//   - reboot=k        on guest `reboot`, invoke the keyboard controller —
//     Firecracker intercepts this and exits cleanly (exit 0).
//   - panic=1         panic immediately on kernel panic instead of hanging.
//   - pci=off         Firecracker doesn't expose a PCI bus; skip probing.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// Spec is the input to Runner.Start. All paths must be absolute and
// readable by the process that runs firecracker.
type Spec struct {
	// Workdir is a per-sandbox directory the runner may create. It will
	// hold api.sock (created by firecracker) and firecracker.log. The
	// caller owns cleanup — the runner does not remove the directory.
	Workdir string

	// Kernel is the path to the guest kernel image (uncompressed vmlinux).
	Kernel string

	// Rootfs is the path to the guest root block device (e.g. rootfs.ext4).
	Rootfs string

	// BootArgs is the kernel command line. Empty means use DefaultBootArgs.
	BootArgs string

	// VCPUs is the guest vCPU count. Must be > 0.
	VCPUs int

	// MemoryMiB is the guest RAM in mebibytes. Must be > 0.
	MemoryMiB int

	// Quotas are host-side cgroup v2 limits the runner applies to the
	// firecracker process tree. Enforced only by runners that wrap
	// firecracker in jailer; the direct-exec runner ignores them.
	//
	// Zero-valued fields mean "no limit on that dimension." Memory
	// and pid limits apply to the VMM's HOST cgroup; guest processes
	// live in a separate pid namespace that the host cgroup can't
	// see, so PIDsMax is a fork-bomb guard for firecracker itself,
	// not for guest code.
	Quotas Quotas

	// NetNS, when non-empty, names the host path of a network
	// namespace Firecracker (under jailer) should join before
	// it starts. Used by the network feature: each sandbox has
	// its own netns set up by internal/network, and this field
	// plumbs that path through to jailer's --netns flag.
	//
	// Only meaningful under JailerRunner. Direct-exec ignores it.
	NetNS string

	// Net, when non-nil, configures Firecracker's virtio-net
	// device post-boot and pre-InstanceStart. Zero value (nil)
	// leaves the VM without a NIC — the default-deny story for
	// the network feature.
	Net *NetConfig
}

// NetConfig describes the guest network interface Firecracker
// attaches. Only populated when the sandbox has network enabled;
// absent means "no NIC at all".
type NetConfig struct {
	// IfaceID is the Firecracker iface_id — always "eth0" in
	// v0.1 since we support exactly one NIC per VM.
	IfaceID string

	// HostDev is the host-side TAP device name (inside the
	// sandbox's netns). Fixed across all sandboxes because
	// snapshot state records this name; having it constant
	// lets forks restore without a host_dev_name rewrite.
	HostDev string

	// GuestMAC is the MAC the guest sees on eth0. Locally-
	// administered (first byte's bit 1 set) so we never collide
	// with real-world OUIs.
	GuestMAC string
}

// Quotas is the cross-runner shape for host-side resource limits.
// Mirrors jailer.Quotas (kept duplicated so the runner package has no
// dependency on jailer internals for the common Spec type).
type Quotas struct {
	// CPUMax is the cpu.max cgroup v2 entry: "<quota> <period>" in
	// microseconds. Example: "20000 100000" caps CPU at 20% of one
	// core. Empty = no CPU limit.
	CPUMax string

	// MemoryMaxBytes maps to memory.max (in bytes). OOM inside the
	// guest when hit; host stays healthy. Zero = no memory limit.
	MemoryMaxBytes int64

	// PIDsMax maps to pids.max on the VMM's HOST cgroup — a cheap
	// fork-bomb guard for firecracker/jailer themselves. Zero = no
	// pids limit.
	PIDsMax int64
}

// Handle is a running (or recently started) Firecracker VM.
//
// Shutdown and Wait may be called from multiple goroutines; both return
// once the firecracker process has exited. Shutdown is idempotent — if
// the process has already exited on its own, Shutdown returns nil.
type Handle interface {
	// Shutdown stops the VM. It attempts a graceful SIGINT first; if ctx
	// expires before the process exits, the runner escalates to SIGKILL.
	Shutdown(ctx context.Context) error

	// Wait blocks until firecracker has exited and returns the exit error
	// (nil on clean exit). Safe to call multiple times.
	Wait() error

	// Workdir returns the per-sandbox workdir this handle is using.
	Workdir() string

	// VSockPath returns the host unix socket path for the guest vsock
	// device. Host-side code opens this socket and speaks Firecracker's
	// hybrid vsock protocol (CONNECT <port>) to reach listeners inside
	// the guest. Empty means the handle has no vsock configured (e.g.
	// test stubs).
	VSockPath() string

	// Pause transitions the VM to Paused. Prerequisite for Snapshot.
	Pause(ctx context.Context) error

	// Resume transitions a Paused VM back to Running. No-op on an
	// already-running VM.
	Resume(ctx context.Context) error

	// Snapshot dumps the VM's state and memory to the given paths. The
	// VM must be Paused before calling; Snapshot does not resume it.
	// statePath is typically shared across forks; memPath and the
	// rootfs file get cloned per-fork by the caller.
	Snapshot(ctx context.Context, statePath, memPath string) error

	// PatchRootfs hot-swaps the rootfs block device to newPath. Used by
	// Manager during snapshot creation (swap to snapshot-owned rootfs
	// before CreateSnapshot so the recorded path is stable) and after
	// Restore (swap to the fork's private rootfs so writes don't
	// corrupt the snapshot). Safe while Paused.
	PatchRootfs(ctx context.Context, newPath string) error
}

// DefaultGuestCID is the vsock CID assigned to every sandbox's guest.
// CID 2 is reserved for the host; we assign 3 to all guests because
// each VM has its own CID namespace — two sandboxes using 3 don't
// collide, and the host distinguishes them by per-sandbox UDS paths.
const DefaultGuestCID = 3

// RestoreSpec is the input to Runner.Restore. Like Spec, the runner
// creates Workdir and writes a log file there. Unlike Spec, kernel /
// rootfs / boot-args / machine-config are *not* required — all of them
// come from the snapshot state file.
type RestoreSpec struct {
	// Workdir is the per-fork directory. The runner creates api.sock,
	// vsock.sock, and firecracker.log inside it.
	Workdir string

	// StatePath is the snapshot state file (written by CreateSnapshot).
	// Typically shared read-only across all forks of the same snapshot.
	StatePath string

	// MemPath is the per-fork memory file. Callers should clone the
	// snapshot's memory file into this path before calling Restore, so
	// each fork has its own writable backing.
	MemPath string

	// RootfsPath is the per-fork writable rootfs file. Firecracker
	// opens the path it saw at snapshot time, so the path here must
	// match the one used by the source sandbox at snapshot time.
	// Callers manage this via symlinks or deliberate path reuse.
	RootfsPath string

	// NetNS, when non-empty, names the host path of a network
	// namespace to place the restored Firecracker process into.
	// Each fork gets its own netns even though they share a
	// snapshot — the recorded TAP name (see NetConfig.HostDev)
	// is the same across netns, so snapshot restore works
	// without host_dev_name rewriting.
	NetNS string
}

// Runner starts Firecracker VMs — either from a cold boot (Start) or
// from a previously-captured snapshot (Restore).
type Runner interface {
	Start(ctx context.Context, spec Spec) (Handle, error)
	Restore(ctx context.Context, spec RestoreSpec) (Handle, error)
}

// ErrInvalidSpec is returned from Start when required fields are missing
// or invalid. Wrap it with %w in callers for errors.Is checks.
var ErrInvalidSpec = errors.New("runner: invalid spec")
