//go:build linux

package main

import (
	"context"
	"os/exec"
	"sort"
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
