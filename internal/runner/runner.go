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
}

// DefaultGuestCID is the vsock CID assigned to every sandbox's guest.
// CID 2 is reserved for the host; we assign 3 to all guests because
// each VM has its own CID namespace — two sandboxes using 3 don't
// collide, and the host distinguishes them by per-sandbox UDS paths.
const DefaultGuestCID = 3

// Runner starts Firecracker VMs.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Handle, error)
}

// ErrInvalidSpec is returned from Start when required fields are missing
// or invalid. Wrap it with %w in callers for errors.Is checks.
var ErrInvalidSpec = errors.New("runner: invalid spec")
