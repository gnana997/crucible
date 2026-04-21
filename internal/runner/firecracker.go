package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/fcapi"
)

const (
	apiSocketName       = "api.sock"
	vsockSocketName     = "vsock.sock"
	logFileName         = "firecracker.log"
	rootfsDriveID       = "rootfs"
	defaultReadyTimeout = 5 * time.Second
	readyPollInterval   = 20 * time.Millisecond
)

// Firecracker is a Runner that launches VMs using the upstream firecracker
// binary. Construct one per daemon; its Start method is goroutine-safe
// because it holds no mutable state.
type Firecracker struct {
	// Binary is the filesystem path to the firecracker executable.
	// Typically /usr/local/bin/firecracker or similar. Required.
	Binary string

	// Logger receives lifecycle events. Nil means use slog.Default.
	Logger *slog.Logger

	// ReadyTimeout bounds how long Start waits for the API socket to
	// answer after firecracker is spawned. Zero means defaultReadyTimeout.
	ReadyTimeout time.Duration
}

// New returns a Firecracker runner that invokes the given binary.
func New(binary string) *Firecracker {
	return &Firecracker{Binary: binary}
}

func (f *Firecracker) logger() *slog.Logger {
	if f.Logger == nil {
		return slog.Default()
	}
	return f.Logger
}

// Start implements Runner.
func (f *Firecracker) Start(ctx context.Context, spec Spec) (Handle, error) {
	if err := f.validate(spec); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(spec.Workdir, 0o750); err != nil {
		return nil, fmt.Errorf("runner: create workdir: %w", err)
	}

	sockPath := filepath.Join(spec.Workdir, apiSocketName)
	vsockPath := filepath.Join(spec.Workdir, vsockSocketName)
	// Remove any stale sockets left by a prior crashed firecracker that
	// shared this workdir. firecracker will refuse to bind otherwise.
	_ = os.Remove(sockPath)
	_ = os.Remove(vsockPath)

	logPath := filepath.Join(spec.Workdir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("runner: open log file: %w", err)
	}

	cmd := exec.Command(f.Binary, "--api-sock", sockPath)
	// Firecracker writes the guest serial console to its own stdout when
	// "console=ttyS0" is set in boot args, so we capture both.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// No guest input for v0.1 (exec support arrives in wk2 via vsock).
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("runner: start firecracker: %w", err)
	}

	log := f.logger().With(
		"component", "runner",
		"workdir", spec.Workdir,
		"pid", cmd.Process.Pid,
	)
	log.Info("firecracker process started")

	h := newHandle(cmd, fcapi.NewClient(sockPath), logFile, spec.Workdir, sockPath, vsockPath, log)

	// Anything past this point can fail; on failure we tear down the handle
	// so the firecracker process and its log file don't leak.
	if err := f.configureAndBoot(ctx, h, spec); err != nil {
		_ = h.Shutdown(context.Background())
		return nil, err
	}

	log.Info("firecracker VM started")
	return h, nil
}

// Restore implements Runner. The flow parallels Start but loads the
// VM state from a snapshot instead of cold-booting:
//
//  1. mkdir workdir; spawn firecracker with a fresh api.sock.
//  2. Wait for the API to respond.
//  3. PUT /vsock — Firecracker rewires vsock to this fork's UDS on load,
//     overriding whatever the snapshot captured.
//  4. PUT /snapshot/load with resume_vm=false — VM is Paused.
//  5. PATCH /drives/rootfs to the fork's per-sandbox rootfs copy —
//     the snapshot state recorded a specific path_on_host, and we
//     swap it so future writes don't corrupt the snapshot's frozen
//     rootfs.
//  6. PATCH /vm {state: Resumed} — fork is live.
//
// The guest agent continues running from where it was at snapshot time
// (its listener socket state was in the VM memory dump). Its host-side
// UDS path is now this fork's vsock.sock, but the agent doesn't know
// anything changed.
func (f *Firecracker) Restore(ctx context.Context, spec RestoreSpec) (Handle, error) {
	if err := f.validateRestore(spec); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(spec.Workdir, 0o750); err != nil {
		return nil, fmt.Errorf("runner: create workdir: %w", err)
	}

	sockPath := filepath.Join(spec.Workdir, apiSocketName)
	vsockPath := filepath.Join(spec.Workdir, vsockSocketName)
	_ = os.Remove(sockPath)
	_ = os.Remove(vsockPath)

	logPath := filepath.Join(spec.Workdir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("runner: open log file: %w", err)
	}

	cmd := exec.Command(f.Binary, "--api-sock", sockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("runner: start firecracker: %w", err)
	}

	log := f.logger().With(
		"component", "runner",
		"mode", "restore",
		"workdir", spec.Workdir,
		"pid", cmd.Process.Pid,
	)
	log.Info("firecracker process started")

	h := newHandle(cmd, fcapi.NewClient(sockPath), logFile, spec.Workdir, sockPath, vsockPath, log)

	if err := f.configureAndLoad(ctx, h, spec); err != nil {
		_ = h.Shutdown(context.Background())
		return nil, err
	}

	log.Info("firecracker VM restored from snapshot")
	return h, nil
}

func (f *Firecracker) configureAndLoad(ctx context.Context, h *fcHandle, spec RestoreSpec) error {
	readyTimeout := f.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = defaultReadyTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := waitReady(readyCtx, h.client); err != nil {
		return fmt.Errorf("runner: wait for API ready: %w", err)
	}

	// Load snapshot FIRST. Firecracker forbids PUT /vsock (and other
	// "boot-specific resource" configuration) before load: the only
	// legal sequence is fresh process → load → post-load reconfigure
	// → resume.
	if err := h.client.LoadSnapshot(ctx, fcapi.SnapshotLoad{
		SnapshotPath: spec.StatePath,
		MemBackend: fcapi.MemBackend{
			BackendType: fcapi.MemBackendFile,
			BackendPath: spec.MemPath,
		},
		ResumeVM: false,
	}); err != nil {
		return fmt.Errorf("runner: load snapshot: %w", err)
	}

	// Rewire vsock AFTER load so the fork binds its own UDS. Without
	// this the fork would try to re-bind the exact host path the
	// source recorded at snapshot time, colliding with the source's
	// still-running firecracker. Under JailerRunner the chroot
	// isolation handles this implicitly; we still issue PUT /vsock
	// there for code uniformity.
	if err := h.client.PutVsock(ctx, fcapi.VsockConfig{
		GuestCID: DefaultGuestCID,
		UDSPath:  h.vsockPath,
	}); err != nil {
		return fmt.Errorf("runner: put vsock: %w", err)
	}

	// Swap the rootfs to the fork's private copy. Without this, the
	// fork would share the snapshot's on-disk rootfs and writes would
	// corrupt the frozen state.
	if err := h.client.PatchDrive(ctx, fcapi.DrivePatch{
		DriveID:    rootfsDriveID,
		PathOnHost: spec.RootfsPath,
	}); err != nil {
		return fmt.Errorf("runner: patch rootfs drive: %w", err)
	}

	if err := h.client.PutVmState(ctx, fcapi.VmStateResumed); err != nil {
		return fmt.Errorf("runner: resume restored VM: %w", err)
	}
	return nil
}

func (f *Firecracker) validateRestore(s RestoreSpec) error {
	if f.Binary == "" {
		return fmt.Errorf("%w: Firecracker.Binary is empty", ErrInvalidSpec)
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

func (f *Firecracker) configureAndBoot(ctx context.Context, h *fcHandle, spec Spec) error {
	readyTimeout := f.ReadyTimeout
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
		KernelImagePath: spec.Kernel,
		BootArgs:        bootArgs,
	}); err != nil {
		return fmt.Errorf("runner: put boot-source: %w", err)
	}
	if err := h.client.PutDrive(ctx, fcapi.Drive{
		DriveID:      rootfsDriveID,
		PathOnHost:   spec.Rootfs,
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
	if err := h.client.PutVsock(ctx, fcapi.VsockConfig{
		GuestCID: DefaultGuestCID,
		UDSPath:  h.vsockPath,
	}); err != nil {
		return fmt.Errorf("runner: put vsock: %w", err)
	}
	if err := h.client.InstanceStart(ctx); err != nil {
		return fmt.Errorf("runner: instance-start: %w", err)
	}
	return nil
}

func (f *Firecracker) validate(s Spec) error {
	if f.Binary == "" {
		return fmt.Errorf("%w: Firecracker.Binary is empty", ErrInvalidSpec)
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

// waitReady polls GetInstanceInfo until it succeeds or ctx is done.
// The socket file may not exist immediately after firecracker is spawned,
// so early polls will fail with ECONNREFUSED or "no such file" — that's
// expected, and we just retry.
func waitReady(ctx context.Context, client *fcapi.Client) error {
	var lastErr error
	for {
		_, err := client.GetInstanceInfo(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last poll error: %v)", ctx.Err(), lastErr)
		case <-time.After(readyPollInterval):
		}
	}
}

// fcHandle implements Handle. It owns the firecracker process, the fcapi
// client, and the workdir artifacts (log file + api socket + vsock socket).
type fcHandle struct {
	cmd       *exec.Cmd
	client    *fcapi.Client
	logFile   *os.File
	workdir   string
	sockPath  string
	vsockPath string
	log       *slog.Logger

	done    chan struct{} // closed when cmd.Wait returns
	waitErr error         // result of cmd.Wait; read only after done is closed

	finalizeOnce sync.Once
}

// newHandle spawns the background goroutine that calls cmd.Wait exactly
// once and publishes its result via the done channel. This lets Shutdown
// and Wait both observe exit without racing on cmd.Wait itself.
func newHandle(cmd *exec.Cmd, client *fcapi.Client, logFile *os.File, workdir, sockPath, vsockPath string, log *slog.Logger) *fcHandle {
	h := &fcHandle{
		cmd:       cmd,
		client:    client,
		logFile:   logFile,
		workdir:   workdir,
		sockPath:  sockPath,
		vsockPath: vsockPath,
		log:       log,
		done:      make(chan struct{}),
	}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.done)
	}()
	return h
}

// Workdir implements Handle.
func (h *fcHandle) Workdir() string { return h.workdir }

// VSockPath implements Handle.
func (h *fcHandle) VSockPath() string { return h.vsockPath }

// Pause implements Handle by delegating to PATCH /vm.
func (h *fcHandle) Pause(ctx context.Context) error {
	return h.client.PutVmState(ctx, fcapi.VmStatePaused)
}

// Resume implements Handle by delegating to PATCH /vm.
func (h *fcHandle) Resume(ctx context.Context) error {
	return h.client.PutVmState(ctx, fcapi.VmStateResumed)
}

// Snapshot implements Handle by delegating to PUT /snapshot/create with
// SnapshotType=Full (v0.1 never emits diff snapshots).
func (h *fcHandle) Snapshot(ctx context.Context, statePath, memPath string) error {
	return h.client.CreateSnapshot(ctx, fcapi.SnapshotCreate{
		SnapshotType: fcapi.SnapshotTypeFull,
		SnapshotPath: statePath,
		MemPath:      memPath,
	})
}

// PatchRootfs implements Handle by delegating to PATCH /drives/rootfs.
func (h *fcHandle) PatchRootfs(ctx context.Context, newPath string) error {
	return h.client.PatchDrive(ctx, fcapi.DrivePatch{
		DriveID:    rootfsDriveID,
		PathOnHost: newPath,
	})
}

// Wait implements Handle. Safe to call multiple times; subsequent calls
// return the same result as the first without re-invoking cmd.Wait.
func (h *fcHandle) Wait() error {
	<-h.done
	h.finalize()
	return h.waitErr
}

// Shutdown implements Handle. It sends SIGINT, then waits up to ctx's
// deadline for firecracker to exit. On ctx expiry it escalates to SIGKILL.
// If firecracker has already exited on its own, Shutdown returns nil.
func (h *fcHandle) Shutdown(ctx context.Context) error {
	// Fast path: process already exited (e.g. guest rebooted with reboot=k).
	select {
	case <-h.done:
		h.finalize()
		return nil
	default:
	}

	if err := h.cmd.Process.Signal(os.Interrupt); err != nil {
		// Typical case: "os: process already finished".
		<-h.done
		h.finalize()
		return nil
	}

	select {
	case <-h.done:
		h.finalize()
		return nil
	case <-ctx.Done():
		h.log.Warn("shutdown deadline exceeded; sending SIGKILL")
		_ = h.cmd.Process.Kill()
		<-h.done
		h.finalize()
		return fmt.Errorf("runner: shutdown timed out, killed process: %w", ctx.Err())
	}
}

// finalize closes the log file and removes the api + vsock sockets.
// Idempotent.
func (h *fcHandle) finalize() {
	h.finalizeOnce.Do(func() {
		_ = h.logFile.Close()
		_ = os.Remove(h.sockPath)
		_ = os.Remove(h.vsockPath)
	})
}

// compile-time assertion that fcHandle satisfies Handle.
var _ Handle = (*fcHandle)(nil)

// compile-time assertion that Firecracker satisfies Runner.
var _ Runner = (*Firecracker)(nil)
