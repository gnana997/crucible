//go:build linux

package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// The reaper waits on wait4(-1), which reaps ALL of the calling
// process's children — so these tests exercise it fully without the
// test process being PID 1. Orphan *reparenting* is the kernel's job
// (only real in a booted init); the reaper's job is to correctly reap
// and dispatch whatever wait4 returns, which is what we test here.

func newTestReaper(t *testing.T) *reaper {
	t.Helper()
	r := newReaper(testLogger())
	r.start()
	t.Cleanup(r.stop)
	return r
}

func TestReaperSpawnExitCode(t *testing.T) {
	r := newTestReaper(t)
	rp, err := r.spawn([]string{"/bin/sh", "-c", "exit 7"}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	res := rp.wait()
	if res.ws.Signaled() {
		t.Fatalf("unexpected signal death: %v", res.ws)
	}
	if res.ws.ExitStatus() != 7 {
		t.Errorf("exit status = %d, want 7", res.ws.ExitStatus())
	}
}

func TestReaperSpawnCapturesOutput(t *testing.T) {
	r := newTestReaper(t)
	var out, errB bytes.Buffer
	rp, err := r.spawn([]string{"/bin/sh", "-c", "echo to-out; echo to-err 1>&2"}, nil, "", nil, &out, &errB)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	rp.wait()
	if strings.TrimSpace(out.String()) != "to-out" {
		t.Errorf("stdout = %q", out.String())
	}
	if strings.TrimSpace(errB.String()) != "to-err" {
		t.Errorf("stderr = %q", errB.String())
	}
}

func TestReaperSignalDeath(t *testing.T) {
	r := newTestReaper(t)
	rp, err := r.spawn([]string{"/bin/sh", "-c", "kill -9 $$"}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	res := rp.wait()
	if !res.ws.Signaled() || res.ws.Signal() != syscall.SIGKILL {
		t.Errorf("want SIGKILL death, got %v", res.ws)
	}
}

func TestReaperSignalGroupStops(t *testing.T) {
	r := newTestReaper(t)
	// A process that ignores nothing but loops; we kill its group.
	rp, err := r.spawn([]string{"/bin/sh", "-c", "while :; do sleep 0.05; done"}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := rp.signal(syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
	}
	res := rp.wait()
	if !res.ws.Signaled() {
		t.Errorf("want signal death after group kill, got %v", res.ws)
	}
}

func TestReaperEnvAndDir(t *testing.T) {
	r := newTestReaper(t)
	dir := t.TempDir()
	var out bytes.Buffer
	rp, err := r.spawn(
		[]string{"/bin/sh", "-c", `printf '%s %s' "$CRUCIBLE_X" "$PWD"`},
		[]string{"CRUCIBLE_X=hello", "PATH=/bin:/usr/bin"}, dir, nil, &out, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	rp.wait()
	if got := out.String(); got != "hello "+dir {
		t.Errorf("output = %q, want %q", got, "hello "+dir)
	}
}

func TestReaperConcurrentSpawns(t *testing.T) {
	// Hammer the reaper: many short-lived children reaped concurrently.
	// Exercises the register/pending race — a child can exit before
	// spawn finishes registering it.
	r := newTestReaper(t)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(code int) {
			defer wg.Done()
			rp, err := r.spawn([]string{"/bin/sh", "-c", "exit " + strconv.Itoa(code)}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil {
				errs <- err
				return
			}
			res := rp.wait()
			if res.ws.ExitStatus() != code {
				errs <- fmt.Errorf("child exit = %d, want %d", res.ws.ExitStatus(), code)
			}
		}(i % 256)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestReaperFastExitNoLostWakeup(t *testing.T) {
	// A child that exits essentially immediately must still be waited
	// on — the pending-map path. Run many to make the race likely.
	r := newTestReaper(t)
	for i := 0; i < 30; i++ {
		rp, err := r.spawn([]string{"/bin/true"}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		done := make(chan waitResult, 1)
		go func() { done <- rp.wait() }()
		select {
		case res := <-done:
			if res.ws.ExitStatus() != 0 {
				t.Fatalf("iter %d: exit = %d, want 0", i, res.ws.ExitStatus())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: wait() never returned (lost wakeup)", i)
		}
	}
}

func TestReaperStartFailure(t *testing.T) {
	r := newTestReaper(t)
	if _, err := r.spawn([]string{"/nonexistent/binary"}, nil, "", nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("spawn of a missing binary succeeded")
	}
}

// ---- initRunner / initChild via the supervisor ------------------------------

func TestInitRunnerSupervisesRealService(t *testing.T) {
	r := newTestReaper(t)
	s := newSupervisor(initRunner{reaper: r}, realClock{}, testLogger(), "")
	t.Cleanup(func() { _, _ = s.Shutdown() })

	startTrapService(t, s, `trap 'exit 0' TERM`)
	if _, err := s.Stop(0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if !st.LastExitRequested {
		t.Error("stop not marked requested")
	}
	if st.LastExit == nil || st.LastExit.ExitCode != 0 {
		t.Errorf("LastExit = %+v, want clean trap exit", st.LastExit)
	}
}

func TestInitRunnerRestartPolicy(t *testing.T) {
	r := newTestReaper(t)
	s := newSupervisor(initRunner{reaper: r}, realClock{}, testLogger(), "")
	t.Cleanup(func() { _, _ = s.Shutdown() })

	spec := specWithPolicy(agentwire.RestartOnFailure, 1, "/bin/sh", "-c", "exit 1")
	if _, err := s.Configure(spec); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateFailed)
	if st.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", st.Restarts)
	}
	if st.LastExit == nil || st.LastExit.ExitCode != 1 {
		t.Errorf("LastExit = %+v, want exit 1", st.LastExit)
	}
}

func TestInitRunnerServiceLogsCaptured(t *testing.T) {
	r := newTestReaper(t)
	s := newSupervisor(initRunner{reaper: r}, realClock{}, testLogger(), "")
	t.Cleanup(func() { _, _ = s.Shutdown() })

	mustConfigureStart(t, s, specWith("/bin/sh", "-c", "echo hello-init; sleep 60"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := s.Logs(0, 1<<20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		var got string
		for _, rec := range resp.Records {
			got += string(rec.Data)
		}
		if strings.Contains(got, "hello-init") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service logs never captured init-mode output")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Stop(time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, s, agentwire.ServiceStateStopped)
}
