//go:build linux

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

func specWithPolicy(policy string, maxRetries int, cmd ...string) *agentwire.ServiceSpec {
	return &agentwire.ServiceSpec{
		Cmd:     cmd,
		Restart: agentwire.RestartPolicy{Policy: policy, MaxRetries: maxRetries},
	}
}

// requireStateStays asserts the supervisor stays in state for a little
// wall-clock while (used after a partial fake-clock advance).
func requireStateStays(t *testing.T, s *supervisor, state string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(deadline) {
		st, err := s.Status()
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State != state {
			t.Fatalf("state = %q, want it to stay %q", st.State, state)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSupervisorAlwaysPolicyRestartsWithDoublingBackoff(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2, fc3 := newFakeChild(101), newFakeChild(102), newFakeChild(103)
	fr.enqueue(fc1, fc2, fc3)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartAlways, 0, "/bin/app"))

	// Crash #1: relaunch after the initial 100ms.
	fc1.exitNow(childExit{ws: exitStatusT(1)})
	waitForState(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial)
	st := waitForState(t, s, agentwire.ServiceStateRunning)
	if st.Pid != 102 {
		t.Fatalf("pid = %d after policy restart, want 102", st.Pid)
	}

	// Crash #2 (quick): the delay doubled, so 100ms is not enough.
	fc2.exitNow(childExit{ws: exitStatusT(1)})
	waitForState(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial)
	requireStateStays(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial) // total 200ms
	st = waitForState(t, s, agentwire.ServiceStateRunning)
	if st.Pid != 103 {
		t.Fatalf("pid = %d, want 103", st.Pid)
	}
	if st.Restarts != 2 {
		t.Errorf("Restarts = %d, want 2", st.Restarts)
	}
}

func TestSupervisorAlwaysPolicyRestartsCleanExit(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2 := newFakeChild(101), newFakeChild(102)
	fr.enqueue(fc1, fc2)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartAlways, 0, "/bin/app"))
	fc1.exitNow(childExit{}) // clean exit — always restarts anyway
	waitForState(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial)
	waitForState(t, s, agentwire.ServiceStateRunning)
}

func TestSupervisorOnFailureIgnoresCleanExit(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101)
	fr.enqueue(fc)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartOnFailure, 0, "/bin/app"))
	fc.exitNow(childExit{}) // exit 0
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.Restarts != 0 {
		t.Errorf("Restarts = %d, want 0", st.Restarts)
	}
	if got := fr.startCount(); got != 1 {
		t.Errorf("start called %d times, want 1", got)
	}
}

func TestSupervisorOnFailureBudgetExhausts(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2, fc3 := newFakeChild(101), newFakeChild(102), newFakeChild(103)
	fr.enqueue(fc1, fc2, fc3)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartOnFailure, 2, "/bin/app"))

	for _, fc := range []*fakeChild{fc1, fc2} {
		fc.exitNow(childExit{ws: exitStatusT(1)})
		waitForState(t, s, agentwire.ServiceStateBackingOff)
		clk.Advance(restartBackoffMax) // generous: fires whatever is armed
		waitForState(t, s, agentwire.ServiceStateRunning)
	}
	// Third consecutive failure: budget (2) exhausted.
	fc3.exitNow(childExit{ws: exitStatusT(1)})
	st := waitForState(t, s, agentwire.ServiceStateFailed)
	if st.Restarts != 2 {
		t.Errorf("Restarts = %d, want 2", st.Restarts)
	}
}

func TestSupervisorStableRunResetsBackoff(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2, fc3 := newFakeChild(101), newFakeChild(102), newFakeChild(103)
	fr.enqueue(fc1, fc2, fc3)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartAlways, 0, "/bin/app"))

	// Quick crash: delay consumed, next would be 200ms.
	fc1.exitNow(childExit{ws: exitStatusT(1), elapsed: 50 * time.Millisecond})
	waitForState(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial)
	waitForState(t, s, agentwire.ServiceStateRunning)

	// This run "survives" past the stability window (reported via its
	// exit duration), so the delay resets to 100ms.
	fc2.exitNow(childExit{ws: exitStatusT(1), elapsed: restartStableAfter + time.Second})
	waitForState(t, s, agentwire.ServiceStateBackingOff)
	clk.Advance(restartBackoffInitial)
	waitForState(t, s, agentwire.ServiceStateRunning)
}

func TestSupervisorStopCancelsBackoff(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101)
	fr.enqueue(fc)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartAlways, 0, "/bin/app"))
	fc.exitNow(childExit{ws: exitStatusT(1)})
	waitForState(t, s, agentwire.ServiceStateBackingOff)

	if _, err := s.Stop(0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.State != agentwire.ServiceStateStopped {
		t.Fatalf("state = %q", st.State)
	}
	clk.Advance(time.Hour)
	requireStateStays(t, s, agentwire.ServiceStateStopped)
	if got := fr.startCount(); got != 1 {
		t.Errorf("start called %d times after cancelled backoff, want 1", got)
	}
}

func TestSupervisorStartDuringBackoffLaunchesNow(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2 := newFakeChild(101), newFakeChild(102)
	fr.enqueue(fc1, fc2)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWithPolicy(agentwire.RestartAlways, 0, "/bin/app"))
	fc1.exitNow(childExit{ws: exitStatusT(1)})
	waitForState(t, s, agentwire.ServiceStateBackingOff)

	st, err := s.Start() // no clock advance needed: explicit start is immediate
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentwire.ServiceStateRunning || st.Pid != 102 {
		t.Fatalf("state=%q pid=%d, want running/102", st.State, st.Pid)
	}
}

func TestSupervisorStartFailureRetriedByPolicy(t *testing.T) {
	fr := &fakeRunner{}
	fr.setStartErr(errors.New("exec: no such file"))
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	if _, err := s.Configure(specWithPolicy(agentwire.RestartOnFailure, 1, "/bin/missing")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	st, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentwire.ServiceStateBackingOff {
		t.Fatalf("state = %q after failing start with policy, want backing_off", st.State)
	}
	clk.Advance(restartBackoffInitial)
	// Second start failure exhausts max_retries=1.
	st = waitForState(t, s, agentwire.ServiceStateFailed)
	if st.LastExit == nil || st.LastExit.Error == "" {
		t.Errorf("LastExit = %+v, want start error", st.LastExit)
	}
}

func TestServiceRealOnFailureExhaustsToFailed(t *testing.T) {
	s := newRealSupervisor(t)
	spec := specWithPolicy(agentwire.RestartOnFailure, 1, "/bin/sh", "-c", "exit 1")
	if _, err := s.Configure(spec); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// crash → 100ms backoff → relaunch → crash → budget exhausted.
	st := waitForState(t, s, agentwire.ServiceStateFailed)
	if st.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", st.Restarts)
	}
	if st.LastExit == nil || st.LastExit.ExitCode != 1 {
		t.Errorf("LastExit = %+v, want exit 1", st.LastExit)
	}
}
