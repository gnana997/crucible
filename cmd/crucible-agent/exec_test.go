//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sort"
	"syscall"
	"testing"
	"time"
)

func TestMergeEnvOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "KEEP=yes"}
	override := map[string]string{
		"PATH": "/opt/bin:/usr/bin",
		"NEW":  "x",
	}
	got := mergeEnv(base, override)
	m := make(map[string]string, len(got))
	for _, kv := range got {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	// Base entry preserved
	if m["HOME"] != "/root" {
		t.Errorf("HOME = %q, want /root", m["HOME"])
	}
	// Override replaced base
	if m["PATH"] != "/opt/bin:/usr/bin" {
		t.Errorf("PATH = %q, want /opt/bin:/usr/bin", m["PATH"])
	}
	// New entry added
	if m["NEW"] != "x" {
		t.Errorf("NEW = %q, want x", m["NEW"])
	}
	// Base-only entry survived
	if m["KEEP"] != "yes" {
		t.Errorf("KEEP = %q, want yes", m["KEEP"])
	}
	// Ensure the output is well-formed (every entry has exactly one '=').
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
}

func TestMergeEnvEmptyOverride(t *testing.T) {
	base := []string{"A=1", "B=2"}
	got := mergeEnv(base, nil)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

func TestCommandContextNoTimeout(t *testing.T) {
	parent := context.Background()
	ctx, cancel := commandContext(parent, 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline when TimeoutSec=0")
	}
}

func TestCommandContextWithTimeout(t *testing.T) {
	parent := context.Background()
	ctx, cancel := commandContext(parent, 1)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline when TimeoutSec>0")
	}
	if remaining := time.Until(d); remaining > time.Second || remaining < 500*time.Millisecond {
		t.Errorf("deadline %v away, want ~1s", remaining)
	}
}

func TestResultFromErrorCleanExit(t *testing.T) {
	cmd := exec.Command("/bin/true")
	runErr := cmd.Run()
	got := resultFromError(runErr, cmd, nil, 5*time.Millisecond)
	if got.ExitCode != 0 || got.Error != "" || got.TimedOut {
		t.Errorf("got %+v, want clean exit", got)
	}
}

func TestResultFromErrorNonZeroExit(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	runErr := cmd.Run()
	got := resultFromError(runErr, cmd, nil, 5*time.Millisecond)
	if got.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", got.ExitCode)
	}
	if got.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestResultFromErrorCommandNotFound(t *testing.T) {
	cmd := exec.Command("/nonexistent/binary/path")
	runErr := cmd.Run()
	got := resultFromError(runErr, cmd, nil, 1*time.Millisecond)
	if got.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", got.ExitCode)
	}
	if got.Error == "" {
		t.Error("Error is empty, want populated")
	}
}

// groupAlive reports whether any process remains in the given process group.
// kill(-pgid, 0) delivers no signal but returns ESRCH once the group is empty.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// waitInBackground runs cmd.Wait in a goroutine and reports whether it returned
// within limit — the core anti-wedge property.
func waitInBackground(cmd *exec.Cmd, limit time.Duration) bool {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// TestExecTimeoutDoesNotWedgeOnInheritedPipe is the primary N4 guard: a command
// that backgrounds a long sleep and exits 0 leaves the sleep holding the
// command's inherited stdout pipe. Without configureExecProcess's WaitDelay,
// cmd.Wait blocks on that pipe until the sleep closes it (effectively forever),
// leaking the handler + pollIO goroutines and the vsock conn. The fix must make
// Wait return within the grace even though the child never closes the pipe.
func TestExecTimeoutDoesNotWedgeOnInheritedPipe(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 999 & exit 0")
	// A non-*os.File writer forces exec to create an internal pipe that the
	// backgrounded sleep inherits — exactly the production frame-writer case.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	configureExecProcess(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := cmd.Process.Pid
	// The orphaned sleep outlives the exec (the leader exited before the
	// deadline, so cancel never fires) — reap the group so the test leaves
	// nothing behind.
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	if !waitInBackground(cmd, execWaitDelay+5*time.Second) {
		t.Fatal("cmd.Wait() wedged past the grace on an inherited stdout pipe")
	}
}

// TestExecTimeoutKillsWholeGroup verifies the second N4 property: when the
// timeout fires while the command is still running, the SIGKILL reaches the
// whole process group, not just the leader. Here bash stays alive on a
// foreground sleep while a second sleep is backgrounded in the same group;
// exec's default cancel (Process.Kill, positive pid) would signal only bash and
// leave both sleeps alive. configureExecProcess's negative-pid kill takes the
// whole group down.
func TestExecTimeoutKillsWholeGroup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 999 & sleep 999")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	configureExecProcess(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }() // belt-and-suspenders

	if !waitInBackground(cmd, execWaitDelay+5*time.Second) {
		t.Fatal("cmd.Wait() did not return after the timeout fired")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}

	// The whole group (bash + both sleeps) must be gone. Poll briefly: after
	// the SIGKILL the members linger as zombies until their reaper collects them.
	deadline := time.Now().Add(5 * time.Second)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatal("process group still alive after timeout — SIGKILL did not reach the whole group")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResultFromErrorTimedOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "5")
	runErr := cmd.Run()
	got := resultFromError(runErr, cmd, ctx.Err(), 60*time.Millisecond)
	if !got.TimedOut {
		t.Errorf("TimedOut = false, want true (ctxErr=%v)", ctx.Err())
	}
	if got.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", got.ExitCode)
	}
	if got.Signal != "SIGKILL" {
		t.Errorf("Signal = %q, want SIGKILL", got.Signal)
	}
}
