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
