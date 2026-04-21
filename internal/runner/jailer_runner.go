package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/fcapi"
	"github.com/gnana997/crucible/internal/fsutil"
	"github.com/gnana997/crucible/internal/jailer"
)

// Chroot-relative paths we stage every VM's files at. These are the
// paths firecracker (inside its pivot_root) sees after jailer sets up
// the chroot. Absolute host paths are computed via jailer.HostPath.
//
// Keeping the layout identical across VMs — rather than, say, using
// the sandbox ID in the name — means every fork sees the same
// /rootfs.ext4 path recorded in the snapshot state, so no drive patch
// is needed at restore time.
const (
	chrootKernelPath = "/vmlinux"
	chrootRootfsPath = "/rootfs.ext4"
	chrootVsockPath  = "/v.sock"
	chrootStatePath  = "/snap.state"
	chrootMemPath    = "/snap.mem"

	// chrootAPISocketPath is where firecracker (inside its chroot)
	// creates its API socket. Jailer doesn't enforce this; it's just
	// firecracker's default when launched without --api-sock. We rely
	// on that default rather than passing a --api-sock override so
	// firecracker and the jailer mount namespace agree.
	chrootAPISocketPath = "/run/firecracker.socket"
)

// JailerRunner is a Runner that launches each firecracker instance
// inside its own jailer chroot. See internal/jailer for the full
// description of what jailer does; the short version is: chroot +
// pivot_root + mount/PID namespaces + cgroup v2 quotas + privilege
// drop to an unprivileged uid.
//
// JailerRunner assumes the daemon is running as root (or has
// CAP_SYS_ADMIN / the specific caps jailer requires) — jailer needs
// to mknod, mount, unshare, and chown before it can drop privileges
// and exec firecracker. If the daemon isn't root, construct a plain
// Firecracker runner instead.
//
// The zero value is not usable; JailerBin, FirecrackerBin, and
// ChrootBase must all be set. UID/GID default to 10000/10000 if
// zero; NewPIDNS defaults to true (via the sentinel field shape
// below).
type JailerRunner struct {
	// JailerBin is the absolute host path to the jailer binary.
	// Required.
	JailerBin string

	// FirecrackerBin is the absolute host path to the firecracker
	// binary jailer should exec after chroot setup. Jailer itself
	// copies this file into the chroot. Required.
	FirecrackerBin string

	// ChrootBase is the parent dir jailer creates per-VM chroots
	// under. Canonical value is /srv/jailer. Required.
	ChrootBase string

	// UID/GID is the unprivileged user jailer drops to before
	// exec'ing firecracker. Files staged into the chroot are
	// chowned to this uid/gid so firecracker can open them.
	UID uint32
	GID uint32

	// DisablePIDNS, if true, suppresses --new-pid-ns. Default
	// behavior (zero value) is to enable PID namespacing — put
	// firecracker in a fresh PID namespace so the VMM is pid 1 in
	// its own world. We use a negative-sense flag here so the
	// struct's zero value is the production-recommended config.
	DisablePIDNS bool

	// Logger receives lifecycle events. Nil means slog.Default.
	Logger *slog.Logger

	// ReadyTimeout bounds how long Start waits for the firecracker
	// API socket to appear inside the chroot. Zero means
	// defaultReadyTimeout.
	ReadyTimeout time.Duration
}

// NewJailerRunner is the standard constructor. uid/gid of zero
// becomes the documented default (10000).
func NewJailerRunner(jailerBin, firecrackerBin, chrootBase string, uid, gid uint32) *JailerRunner {
	if uid == 0 {
		uid = 10000
	}
	if gid == 0 {
		gid = 10000
	}
	return &JailerRunner{
		JailerBin:      jailerBin,
		FirecrackerBin: firecrackerBin,
		ChrootBase:     chrootBase,
		UID:            uid,
		GID:            gid,
	}
}

func (j *JailerRunner) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.Default()
	}
	return j.Logger
}

// Start implements Runner. The flow:
//
//  1. Validate spec + runner config.
//  2. Derive a jailer ID from Workdir's basename (sanitized: jailer
//     forbids underscores, which our sandbox IDs contain).
//  3. jailer.Stage the kernel + rootfs into the chroot at
//     chrootKernelPath / chrootRootfsPath.
//  4. exec jailer, which eventually execs firecracker inside its
//     pivot_root.
//  5. Poll the API socket at HostPath(chrootAPISocketPath) until
//     firecracker answers GET /.
//  6. Configure boot-source, drive, machine-config, vsock — all
//     with CHROOT-RELATIVE paths, because firecracker sees them
//     after pivot_root.
//  7. InstanceStart.
//  8. Return a handle that, on shutdown, also runs jailer.Cleanup
//     to remove the chroot + cgroup.
func (j *JailerRunner) Start(ctx context.Context, spec Spec) (Handle, error) {
	if err := j.validateStart(spec); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(spec.Workdir, 0o750); err != nil {
		return nil, fmt.Errorf("runner: create workdir: %w", err)
	}

	jSpec, err := j.buildJailerSpec(spec.Workdir, spec.Quotas)
	if err != nil {
		return nil, err
	}

	// Stage kernel + rootfs. Hardlink-first (same-filesystem: no IO);
	// FICLONE or byte-copy fallback on cross-device.
	if err := jailer.Stage(jSpec, map[string]string{
		chrootKernelPath: spec.Kernel,
		chrootRootfsPath: spec.Rootfs,
	}); err != nil {
		return nil, fmt.Errorf("runner: stage files for jailer: %w", err)
	}

	cmd, logFile, err := j.launch(jSpec, spec.Workdir)
	if err != nil {
		_ = jailer.Cleanup(jSpec)
		return nil, err
	}

	sockPath := jailer.HostPath(jSpec, chrootAPISocketPath)
	vsockHostPath := jailer.HostPath(jSpec, chrootVsockPath)

	log := j.logger().With(
		"component", "runner",
		"mode", "jailer",
		"workdir", spec.Workdir,
		"chroot", jailer.ChrootRoot(jSpec),
		"pid", cmd.Process.Pid,
	)
	log.Info("jailer process started")

	fch := newHandle(cmd, fcapi.NewClient(sockPath), logFile, spec.Workdir, sockPath, vsockHostPath, log)
	h := &jailerHandle{fcHandle: fch, jailerSpec: jSpec}

	// Anything past here can fail; tear down the handle (and by
	// extension the chroot) on any error.
	if err := j.configureAndBoot(ctx, h.fcHandle, spec); err != nil {
		_ = h.Shutdown(context.Background())
		return nil, err
	}

	log.Info("jailer VM started")
	return h, nil
}

// Restore implements Runner. Snapshot state, memory, and rootfs files
// are staged into the fork's chroot at well-known chroot-relative
// paths; those paths are then passed to LoadSnapshot / PatchDrive.
// Because each fork gets its own chroot, each fork's vsock UDS lives
// at a unique absolute host path — the whole reason we adopted jailer.
func (j *JailerRunner) Restore(ctx context.Context, spec RestoreSpec) (Handle, error) {
	if err := j.validateRestore(spec); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(spec.Workdir, 0o750); err != nil {
		return nil, fmt.Errorf("runner: create workdir: %w", err)
	}

	jSpec, err := j.buildJailerSpec(spec.Workdir, Quotas{})
	if err != nil {
		return nil, err
	}

	if err := jailer.Stage(jSpec, map[string]string{
		chrootStatePath:  spec.StatePath,
		chrootMemPath:    spec.MemPath,
		chrootRootfsPath: spec.RootfsPath,
	}); err != nil {
		return nil, fmt.Errorf("runner: stage snapshot files for jailer: %w", err)
	}

	cmd, logFile, err := j.launch(jSpec, spec.Workdir)
	if err != nil {
		_ = jailer.Cleanup(jSpec)
		return nil, err
	}

	sockPath := jailer.HostPath(jSpec, chrootAPISocketPath)
	vsockHostPath := jailer.HostPath(jSpec, chrootVsockPath)

	log := j.logger().With(
		"component", "runner",
		"mode", "jailer-restore",
		"workdir", spec.Workdir,
		"chroot", jailer.ChrootRoot(jSpec),
		"pid", cmd.Process.Pid,
	)
	log.Info("jailer process started")

	fch := newHandle(cmd, fcapi.NewClient(sockPath), logFile, spec.Workdir, sockPath, vsockHostPath, log)
	h := &jailerHandle{fcHandle: fch, jailerSpec: jSpec}

	if err := j.configureAndLoad(ctx, h.fcHandle); err != nil {
		_ = h.Shutdown(context.Background())
		return nil, err
	}

	log.Info("jailer VM restored from snapshot")
	return h, nil
}

// launch execs jailer with the computed argv and hooks its stdout/
// stderr to a host-side log file. We deliberately do NOT pass
// --daemonize so jailer stays attached to this exec.Cmd — cmd.Wait()
// returns when jailer exits, which (with --new-pid-ns) happens when
// firecracker exits.
func (j *JailerRunner) launch(jSpec jailer.Spec, workdir string) (*exec.Cmd, *os.File, error) {
	logPath := filepath.Join(workdir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: open log file: %w", err)
	}

	args := jailer.BuildArgs(jSpec, nil)
	cmd := exec.Command(j.JailerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, fmt.Errorf("runner: start jailer: %w", err)
	}
	return cmd, logFile, nil
}

// buildJailerSpec turns a Workdir path + Quotas into a jailer.Spec.
// The jailer ID is derived from the workdir's basename (which the
// sandbox manager sets to the sandbox ID, e.g. "sbx_abc") and
// sanitized: jailer's regex forbids underscores.
func (j *JailerRunner) buildJailerSpec(workdir string, q Quotas) (jailer.Spec, error) {
	id := sanitizeJailerID(filepath.Base(workdir))
	spec := jailer.Spec{
		ID:         id,
		ExecFile:   j.FirecrackerBin,
		UID:        j.UID,
		GID:        j.GID,
		ChrootBase: j.ChrootBase,
		NewPIDNS:   !j.DisablePIDNS,
		Quotas: jailer.Quotas{
			CPUMax:         q.CPUMax,
			MemoryMaxBytes: q.MemoryMaxBytes,
			PIDsMax:        q.PIDsMax,
		},
	}
	if err := spec.Validate(); err != nil {
		return jailer.Spec{}, fmt.Errorf("runner: %w", err)
	}
	return spec, nil
}

// sanitizeJailerID replaces characters jailer's regex forbids with a
// hyphen. Sandbox IDs use `sbx_<base32>` shape; jailer accepts
// `[a-zA-Z0-9-]{1,64}`. The single underscore between prefix and
// suffix is the only problematic character we produce, so this is a
// one-rule swap rather than a general-purpose sanitizer.
func sanitizeJailerID(id string) string {
	return strings.ReplaceAll(id, "_", "-")
}

// configureAndBoot is the cold-boot fcapi sequence, issued against
// the in-chroot firecracker with chroot-relative paths. The shape
// mirrors Firecracker.configureAndBoot but every path is the
// chroot-relative constant so firecracker sees it correctly after
// pivot_root.
func (j *JailerRunner) configureAndBoot(ctx context.Context, h *fcHandle, spec Spec) error {
	readyTimeout := j.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = defaultReadyTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := waitReady(readyCtx, h.client); err != nil {
		return fmt.Errorf("runner: wait for API ready: %w", err)
	}

	bootArgs := spec.BootArgs
	if bootArgs == "" {
		bootArgs = DefaultBootArgs
	}

	if err := h.client.PutBootSource(ctx, fcapi.BootSource{
		KernelImagePath: chrootKernelPath,
		BootArgs:        bootArgs,
	}); err != nil {
		return fmt.Errorf("runner: put boot-source: %w", err)
	}
	if err := h.client.PutDrive(ctx, fcapi.Drive{
		DriveID:      rootfsDriveID,
		PathOnHost:   chrootRootfsPath,
		IsRootDevice: true,
	}); err != nil {
		return fmt.Errorf("runner: put rootfs drive: %w", err)
	}
	if err := h.client.PutMachineConfig(ctx, fcapi.MachineConfig{
		VCPUCount:  spec.VCPUs,
		MemSizeMiB: spec.MemoryMiB,
	}); err != nil {
		return fmt.Errorf("runner: put machine-config: %w", err)
	}
	// Vsock UDS is chroot-relative here, but will resolve to a unique
	// absolute host path for every VM (each has its own chroot).
	// That uniqueness is what makes fork work under v1.15 without
	// the upstream vsock_override field.
	if err := h.client.PutVsock(ctx, fcapi.VsockConfig{
		GuestCID: DefaultGuestCID,
		UDSPath:  chrootVsockPath,
	}); err != nil {
		return fmt.Errorf("runner: put vsock: %w", err)
	}
	if err := h.client.InstanceStart(ctx); err != nil {
		return fmt.Errorf("runner: instance-start: %w", err)
	}
	return nil
}

// configureAndLoad is the restore fcapi sequence. Ordering matters:
// LoadSnapshot first (firecracker forbids PUT /vsock before load),
// then rewire vsock (each fork gets its own chroot-relative path that
// resolves to a unique absolute host path), then drive patch (so the
// fork's rootfs writes don't corrupt the snapshot's frozen rootfs),
// then resume.
func (j *JailerRunner) configureAndLoad(ctx context.Context, h *fcHandle) error {
	readyTimeout := j.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = defaultReadyTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := waitReady(readyCtx, h.client); err != nil {
		return fmt.Errorf("runner: wait for API ready: %w", err)
	}

	if err := h.client.LoadSnapshot(ctx, fcapi.SnapshotLoad{
		SnapshotPath: chrootStatePath,
		MemBackend: fcapi.MemBackend{
			BackendType: fcapi.MemBackendFile,
			BackendPath: chrootMemPath,
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("runner: load snapshot: %w", err)
	}

	if err := h.client.PutVsock(ctx, fcapi.VsockConfig{
		GuestCID: DefaultGuestCID,
		UDSPath:  chrootVsockPath,
	}); err != nil {
		return fmt.Errorf("runner: put vsock: %w", err)
	}

	if err := h.client.PatchDrive(ctx, fcapi.DrivePatch{
		DriveID:    rootfsDriveID,
		PathOnHost: chrootRootfsPath,
	}); err != nil {
		return fmt.Errorf("runner: patch rootfs drive: %w", err)
	}

	if err := h.client.PutVmState(ctx, fcapi.VmStateResumed); err != nil {
		return fmt.Errorf("runner: resume restored VM: %w", err)
	}
	return nil
}

func (j *JailerRunner) validateStart(s Spec) error {
	if err := j.validateSelf(); err != nil {
		return err
	}
	if s.Workdir == "" {
		return fmt.Errorf("%w: Workdir is required", ErrInvalidSpec)
	}
	if s.Kernel == "" {
		return fmt.Errorf("%w: Kernel is required", ErrInvalidSpec)
	}
	if s.Rootfs == "" {
		return fmt.Errorf("%w: Rootfs is required", ErrInvalidSpec)
	}
	if s.VCPUs <= 0 {
		return fmt.Errorf("%w: VCPUs must be > 0", ErrInvalidSpec)
	}
	if s.MemoryMiB <= 0 {
		return fmt.Errorf("%w: MemoryMiB must be > 0", ErrInvalidSpec)
	}
	return nil
}

func (j *JailerRunner) validateRestore(s RestoreSpec) error {
	if err := j.validateSelf(); err != nil {
		return err
	}
	if s.Workdir == "" {
		return fmt.Errorf("%w: Workdir is required", ErrInvalidSpec)
	}
	if s.StatePath == "" {
		return fmt.Errorf("%w: StatePath is required", ErrInvalidSpec)
	}
	if s.MemPath == "" {
		return fmt.Errorf("%w: MemPath is required", ErrInvalidSpec)
	}
	if s.RootfsPath == "" {
		return fmt.Errorf("%w: RootfsPath is required", ErrInvalidSpec)
	}
	return nil
}

func (j *JailerRunner) validateSelf() error {
	if j.JailerBin == "" {
		return fmt.Errorf("%w: JailerRunner.JailerBin is empty", ErrInvalidSpec)
	}
	if j.FirecrackerBin == "" {
		return fmt.Errorf("%w: JailerRunner.FirecrackerBin is empty", ErrInvalidSpec)
	}
	if j.ChrootBase == "" {
		return fmt.Errorf("%w: JailerRunner.ChrootBase is empty", ErrInvalidSpec)
	}
	return nil
}

// jailerHandle wraps fcHandle with post-exit chroot/cgroup teardown.
// Every path that drives the handle to terminal state (Shutdown, Wait)
// funnels through cleanupJailer via sync.Once so the chroot is
// removed exactly once regardless of how many callers observe exit.
type jailerHandle struct {
	*fcHandle

	jailerSpec  jailer.Spec
	cleanupOnce sync.Once
}

func (h *jailerHandle) Shutdown(ctx context.Context) error {
	err := h.fcHandle.Shutdown(ctx)
	h.cleanupJailer()
	return err
}

func (h *jailerHandle) Wait() error {
	err := h.fcHandle.Wait()
	h.cleanupJailer()
	return err
}

// Snapshot shadows the fcHandle.Snapshot promoted method. Firecracker,
// sitting inside a pivot_root, can only write files at chroot-relative
// paths — so we tell it to write to well-known chroot paths and then
// move the resulting files out to the caller's requested host-
// absolute paths.
//
// The "move" is os.Rename when both paths share a filesystem and a
// Clone+remove fallback otherwise. See fsutil.Move.
func (h *jailerHandle) Snapshot(ctx context.Context, statePath, memPath string) error {
	if err := h.client.CreateSnapshot(ctx, fcapi.SnapshotCreate{
		SnapshotType: fcapi.SnapshotTypeFull,
		SnapshotPath: chrootStatePath,
		MemPath:      chrootMemPath,
	}); err != nil {
		return fmt.Errorf("runner: create snapshot: %w", err)
	}

	hostState := jailer.HostPath(h.jailerSpec, chrootStatePath)
	hostMem := jailer.HostPath(h.jailerSpec, chrootMemPath)

	if err := fsutil.Move(hostState, statePath); err != nil {
		return fmt.Errorf("runner: move snapshot state out of chroot: %w", err)
	}
	if err := fsutil.Move(hostMem, memPath); err != nil {
		return fmt.Errorf("runner: move snapshot memory out of chroot: %w", err)
	}
	return nil
}

func (h *jailerHandle) cleanupJailer() {
	h.cleanupOnce.Do(func() {
		if err := jailer.Cleanup(h.jailerSpec); err != nil {
			h.fcHandle.log.Warn("jailer cleanup failed", "err", err)
		}
	})
}

// compile-time assertion that JailerRunner satisfies Runner.
var _ Runner = (*JailerRunner)(nil)

// compile-time assertion that jailerHandle satisfies Handle.
var _ Handle = (*jailerHandle)(nil)
