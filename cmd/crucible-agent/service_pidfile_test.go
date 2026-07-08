//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestProcStartTimeSelf(t *testing.T) {
	start, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("procStartTime(self): %v", err)
	}
	if start == 0 {
		t.Error("start time = 0, want > 0")
	}
}

func TestReconcileStaleServiceKillsLiveOrphan(t *testing.T) {
	// Simulate the orphan: a real process in its own group, recorded in
	// a pidfile, that the "restarted agent" must kill.
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := cmd.Process.Pid
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	path := filepath.Join(t.TempDir(), "service.pid")
	if err := writeServicePidFile(path, pid); err != nil {
		t.Fatalf("writeServicePidFile: %v", err)
	}

	reconcileStaleService(path, testLogger())

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("orphan not killed by reconcile")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present after reconcile: %v", err)
	}
}

func TestReconcileStaleServiceDeadPid(t *testing.T) {
	// A pid whose process has exited: reconcile must remove the file
	// and kill nothing.
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	start, err := procStartTime(pid)
	if err != nil {
		// The process may already be gone; use a fabricated start time.
		start = 12345
	}
	_ = cmd.Wait()

	path := filepath.Join(t.TempDir(), "service.pid")
	if err := os.WriteFile(path, []byte(formatPidFile(pid, start)), 0o644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	reconcileStaleService(path, testLogger())
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present: %v", err)
	}
}

func TestReconcileStaleServiceMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.pid")
	if err := os.WriteFile(path, []byte("not a pidfile"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	reconcileStaleService(path, testLogger()) // must not panic
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("malformed pidfile not removed: %v", err)
	}
}

func TestReconcileStaleServiceNoFile(t *testing.T) {
	reconcileStaleService(filepath.Join(t.TempDir(), "absent.pid"), testLogger())
}

func formatPidFile(pid int, start uint64) string {
	return strconv.Itoa(pid) + " " + strconv.FormatUint(start, 10) + "\n"
}
