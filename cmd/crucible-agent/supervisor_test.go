//go:build linux

package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// ---- fakes ----------------------------------------------------------------

// fakeClock is a manually-advanced clock. After registers a waiter that
// fires when Advance moves now past its deadline. Command handling in
// the supervisor is synchronous (reply after the timer is armed), so a
// test that calls Advance after a command returned cannot race the
// registration.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining
}

// fakeChild is a controllable serviceChild. Tests deliver the exit via
// exitNow (or automatically through onSignal hooks). Like a real
// process, it always dies on SIGKILL — that keeps shutdown paths
// realistic and cleanup deadlock-free.
type fakeChild struct {
	id     int
	waitCh chan childExit

	mu       sync.Mutex
	signals  []syscall.Signal
	onSignal func(sig syscall.Signal)
	exited   bool
}

func newFakeChild(id int) *fakeChild {
	return &fakeChild{id: id, waitCh: make(chan childExit, 1)}
}

func (c *fakeChild) pid() int { return c.id }

func (c *fakeChild) signalGroup(sig syscall.Signal) error {
	c.mu.Lock()
	c.signals = append(c.signals, sig)
	hook := c.onSignal
	c.mu.Unlock()
	if hook != nil {
		hook(sig)
	}
	if sig == syscall.SIGKILL {
		c.exitNow(childExit{})
	}
	return nil
}

func (c *fakeChild) wait() childExit { return <-c.waitCh }

// exitNow delivers the exit exactly once; later calls are ignored (a
// process only dies once).
func (c *fakeChild) exitNow(e childExit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return
	}
	c.exited = true
	c.waitCh <- e
}

// exitOn makes the child deliver exit e when it receives signal sig
// (e.g. a well-behaved process exiting on SIGTERM).
func (c *fakeChild) exitOn(sig syscall.Signal, e childExit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSignal = func(got syscall.Signal) {
		if got == sig {
			c.exitNow(e)
		}
	}
}

func (c *fakeChild) gotSignal(sig syscall.Signal) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.signals {
		if s == sig {
			return true
		}
	}
	return false
}

// fakeRunner hands out pre-queued fakeChildren in order and captures
// the writers each start received, so tests can feed the log ring.
type fakeRunner struct {
	mu         sync.Mutex
	queue      []*fakeChild
	started    []agentwire.ServiceSpec
	startErr   error
	lastStdout io.Writer
	lastStderr io.Writer
}

func (r *fakeRunner) start(spec *agentwire.ServiceSpec, stdout, stderr io.Writer) (serviceChild, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	if len(r.queue) == 0 {
		return nil, errors.New("fakeRunner: no queued child")
	}
	r.started = append(r.started, *spec)
	r.lastStdout, r.lastStderr = stdout, stderr
	c := r.queue[0]
	r.queue = r.queue[1:]
	return c, nil
}

func (r *fakeRunner) writers() (stdout, stderr io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastStdout, r.lastStderr
}

func (r *fakeRunner) setStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

func (r *fakeRunner) enqueue(children ...*fakeChild) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, children...)
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *fakeRunner) startedSpec(i int) agentwire.ServiceSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started[i]
}

// ---- helpers ---------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestSupervisor(t *testing.T, runner childRunner, clk clock) *supervisor {
	t.Helper()
	s := newSupervisor(runner, clk, testLogger(), "")
	t.Cleanup(func() {
		// Shutdown may need the stop-grace timer to fire (a child that
		// ignores the stop signal is only killed after the grace), so
		// pump a fake clock while waiting. SIGKILL always terminates a
		// fakeChild, mirroring a real process.
		done := make(chan struct{})
		go func() { _, _ = s.Shutdown(); close(done) }()
		fc, _ := clk.(*fakeClock)
		deadline := time.Now().Add(5 * time.Second)
		for {
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
				if fc != nil {
					fc.Advance(time.Minute)
				}
				if time.Now().After(deadline) {
					t.Error("supervisor shutdown did not complete in cleanup")
					return
				}
			}
		}
	})
	return s
}

func specWith(cmd ...string) *agentwire.ServiceSpec {
	return &agentwire.ServiceSpec{Cmd: cmd}
}

// waitForState polls Status until the state matches or a deadline
// passes. Exits from the child are delivered asynchronously through the
// watcher goroutine, so tests can't assert the post-exit state
// synchronously.
func waitForState(t *testing.T, s *supervisor, state string) agentwire.ServiceStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := s.Status()
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want %q (timed out)", st.State, state)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitForSignal(t *testing.T, c *fakeChild, sig syscall.Signal) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.gotSignal(sig) {
		if time.Now().After(deadline) {
			t.Fatalf("child never received %v", sig)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- state machine tests (fakes) -------------------------------------------

func TestSupervisorConfigureNormalizesAndStops(t *testing.T) {
	fr := &fakeRunner{}
	s := newTestSupervisor(t, fr, newFakeClock())

	st, err := s.Configure(specWith("/bin/app"))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if st.State != agentwire.ServiceStateStopped {
		t.Errorf("state = %q, want stopped", st.State)
	}
	if st.Spec == nil || st.Spec.StopSignal != "SIGTERM" || st.Spec.StopGraceSec != 10 {
		t.Errorf("spec not normalized: %+v", st.Spec)
	}
	if st.Spec.Restart.Policy != agentwire.RestartNever {
		t.Errorf("restart policy = %q, want never", st.Spec.Restart.Policy)
	}
}

func TestSupervisorConfigureRejectsBadSpec(t *testing.T) {
	s := newTestSupervisor(t, &fakeRunner{}, newFakeClock())

	if _, err := s.Configure(&agentwire.ServiceSpec{}); err == nil {
		t.Error("empty cmd accepted")
	}
	if _, err := s.Configure(&agentwire.ServiceSpec{Cmd: []string{"/x"}, StopSignal: "SIGNOPE"}); err == nil {
		t.Error("unknown stop signal accepted")
	}
}

func TestSupervisorStartWithoutSpec(t *testing.T) {
	s := newTestSupervisor(t, &fakeRunner{}, newFakeClock())
	if _, err := s.Start(); !errors.Is(err, errNoServiceSpec) {
		t.Fatalf("err = %v, want errNoServiceSpec", err)
	}
	if _, err := s.Restart(); !errors.Is(err, errNoServiceSpec) {
		t.Fatalf("restart err = %v, want errNoServiceSpec", err)
	}
}

func TestSupervisorStartStopLifecycle(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101)
	fc.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	if _, err := s.Configure(specWith("/bin/app", "--serve")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	st, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentwire.ServiceStateRunning || st.Pid != 101 {
		t.Fatalf("after start: state=%q pid=%d", st.State, st.Pid)
	}

	if _, err := s.Stop(0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st = waitForState(t, s, agentwire.ServiceStateStopped)
	if !st.LastExitRequested {
		t.Error("LastExitRequested = false, want true")
	}
	if st.LastExit == nil || st.LastExit.ExitCode != 0 {
		t.Errorf("LastExit = %+v, want clean exit", st.LastExit)
	}
	if st.Pid != 0 {
		t.Errorf("pid = %d after stop, want 0", st.Pid)
	}
}

func TestSupervisorIdempotentStartAndStop(t *testing.T) {
	fr := &fakeRunner{}
	fr.enqueue(newFakeChild(101))
	s := newTestSupervisor(t, fr, newFakeClock())

	// Stop before anything is configured: no-op, no error.
	if _, err := s.Stop(0); err != nil {
		t.Fatalf("stop when idle: %v", err)
	}

	if _, err := s.Configure(specWith("/bin/app")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := s.Start() // second start: no-op
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if st.State != agentwire.ServiceStateRunning {
		t.Errorf("state = %q, want running", st.State)
	}
	if got := fr.startCount(); got != 1 {
		t.Errorf("runner.start called %d times, want 1", got)
	}
}

func TestSupervisorSelfExitIsNotRequested(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101)
	fr.enqueue(fc)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app"))
	fc.exitNow(childExit{ws: exitStatusT(42), elapsed: time.Second})

	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.LastExitRequested {
		t.Error("LastExitRequested = true for a self-exit")
	}
	// A self-exit reports its real exit code, not a synthetic error.
	if st.LastExit == nil || st.LastExit.ExitCode != 42 {
		t.Errorf("LastExit = %+v, want exit code 42", st.LastExit)
	}
}

// exitStatusT builds a WaitStatus for a normal exit with the given code
// (Linux encodes the exit code in bits 8–15). signalStatusT builds one
// for a signal death (low 7 bits = signal number).
func exitStatusT(code int) syscall.WaitStatus {
	return syscall.WaitStatus(uint32(code&0xff) << 8)
}

func signalStatusT(sig syscall.Signal) syscall.WaitStatus {
	return syscall.WaitStatus(uint32(sig) & 0x7f)
}

// TestServiceResultSignalConvention pins the 128+n exit-code mapping and
// SIGNAME-style signal name for a signal death — the supervised-service
// contract, independent of the runner.
func TestServiceResultSignalConvention(t *testing.T) {
	res := serviceResult(childExit{ws: signalStatusT(syscall.SIGKILL)}, false)
	if res.ExitCode != 137 || res.Signal != "SIGKILL" {
		t.Errorf("SIGKILL death = code %d signal %q, want 137 SIGKILL", res.ExitCode, res.Signal)
	}
	res = serviceResult(childExit{ws: exitStatusT(3)}, false)
	if res.ExitCode != 3 || res.Signal != "" {
		t.Errorf("exit 3 = code %d signal %q, want 3 (no signal)", res.ExitCode, res.Signal)
	}
}

func TestSupervisorGraceEscalation(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101) // ignores SIGTERM
	fr.enqueue(fc)
	clk := newFakeClock()
	s := newTestSupervisor(t, fr, clk)

	mustConfigureStart(t, s, specWith("/bin/app"))
	st, err := s.Stop(0)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st.State != agentwire.ServiceStateStopping {
		t.Fatalf("state = %q, want stopping", st.State)
	}
	waitForSignal(t, fc, syscall.SIGTERM)

	clk.Advance(10 * time.Second) // the normalized default grace
	waitForSignal(t, fc, syscall.SIGKILL)

	fc.exitNow(childExit{})
	waitForState(t, s, agentwire.ServiceStateStopped)
}

func TestSupervisorRestartRelaunches(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2 := newFakeChild(101), newFakeChild(102)
	fc1.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc1, fc2)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app"))
	if _, err := s.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateRunning)
	if st.Pid != 102 {
		t.Errorf("pid = %d after restart, want 102", st.Pid)
	}
	if got := fr.startCount(); got != 2 {
		t.Errorf("runner.start called %d times, want 2", got)
	}
	if !st.LastExitRequested {
		t.Error("restart exit not marked requested")
	}
}

func TestSupervisorRespecWhileRunningRelaunchesWithNewSpec(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2 := newFakeChild(101), newFakeChild(102)
	fc1.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc1, fc2)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app", "v1"))
	st, err := s.Configure(specWith("/bin/app", "v2"))
	if err != nil {
		t.Fatalf("re-Configure: %v", err)
	}
	if st.State != agentwire.ServiceStateStopping {
		t.Fatalf("state = %q, want stopping", st.State)
	}
	st = waitForState(t, s, agentwire.ServiceStateRunning)
	if st.Pid != 102 {
		t.Errorf("pid = %d, want 102", st.Pid)
	}
	if got := fr.startedSpec(1).Cmd[1]; got != "v2" {
		t.Errorf("relaunched with cmd arg %q, want v2", got)
	}
}

func TestSupervisorRespecWhileStoppedSwapsInPlace(t *testing.T) {
	fr := &fakeRunner{}
	s := newTestSupervisor(t, fr, newFakeClock())

	if _, err := s.Configure(specWith("/bin/app", "v1")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	st, err := s.Configure(specWith("/bin/app", "v2"))
	if err != nil {
		t.Fatalf("re-Configure: %v", err)
	}
	if st.State != agentwire.ServiceStateStopped {
		t.Errorf("state = %q, want stopped", st.State)
	}
	if got := st.Spec.Cmd[1]; got != "v2" {
		t.Errorf("spec cmd arg = %q, want v2", got)
	}
	if fr.startCount() != 0 {
		t.Error("configure alone must not start the service")
	}
}

func TestSupervisorStopCancelsPendingRestart(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101) // ignores SIGTERM so the stop stays in flight
	fr.enqueue(fc)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app"))
	if _, err := s.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, err := s.Stop(0); err != nil { // stop wins over the pending relaunch
		t.Fatalf("Stop: %v", err)
	}
	fc.exitNow(childExit{})
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if got := fr.startCount(); got != 1 {
		t.Errorf("runner.start called %d times, want 1 (restart cancelled)", got)
	}
	if !st.LastExitRequested {
		t.Error("stop exit not marked requested")
	}
}

func TestSupervisorStartFailureIsFailedState(t *testing.T) {
	fr := &fakeRunner{}
	fr.setStartErr(errors.New("exec: no such file"))
	s := newTestSupervisor(t, fr, newFakeClock())

	if _, err := s.Configure(specWith("/bin/missing")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	st, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentwire.ServiceStateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if st.LastExit == nil || st.LastExit.Error == "" {
		t.Errorf("LastExit = %+v, want start error recorded", st.LastExit)
	}

	// The host can retry after fixing the problem.
	fr.setStartErr(nil)
	fr.enqueue(newFakeChild(101))
	st, err = s.Start()
	if err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	if st.State != agentwire.ServiceStateRunning {
		t.Errorf("state = %q after retry, want running", st.State)
	}
}

func TestSupervisorShutdownStopsService(t *testing.T) {
	fr := &fakeRunner{}
	fc := newFakeChild(101)
	fc.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc)
	s := newSupervisor(fr, newFakeClock(), testLogger(), "")

	mustConfigureStart(t, s, specWith("/bin/app"))
	st, err := s.Shutdown()
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if st.State != agentwire.ServiceStateStopped {
		t.Errorf("state = %q, want stopped", st.State)
	}
	if _, err := s.Status(); !errors.Is(err, errSupervisorDown) {
		t.Errorf("Status after shutdown: err = %v, want errSupervisorDown", err)
	}
}

func TestSupervisorShutdownWhenIdle(t *testing.T) {
	s := newSupervisor(&fakeRunner{}, newFakeClock(), testLogger(), "")
	st, err := s.Shutdown()
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if st.State != agentwire.ServiceStateIdle {
		t.Errorf("state = %q, want idle", st.State)
	}
	if _, err := s.Start(); !errors.Is(err, errSupervisorDown) {
		t.Errorf("Start after shutdown: err = %v, want errSupervisorDown", err)
	}
}

func mustConfigureStart(t *testing.T, s *supervisor, spec *agentwire.ServiceSpec) {
	t.Helper()
	if _, err := s.Configure(spec); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	st, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentwire.ServiceStateRunning {
		t.Fatalf("state after start = %q, want running", st.State)
	}
}

// ---- real-process tests (execRunner) ---------------------------------------

func newRealSupervisor(t *testing.T) *supervisor {
	t.Helper()
	s := newSupervisor(execRunner{}, realClock{}, testLogger(), "")
	t.Cleanup(func() { _, _ = s.Shutdown() })
	return s
}

func TestServiceRealExitCode(t *testing.T) {
	s := newRealSupervisor(t)
	mustConfigureStart(t, s, specWith("/bin/sh", "-c", "exit 3"))
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.LastExit == nil || st.LastExit.ExitCode != 3 {
		t.Errorf("LastExit = %+v, want exit code 3", st.LastExit)
	}
	if st.LastExitRequested {
		t.Error("self-exit marked requested")
	}
}

func TestServiceRealSignalDeathReports128PlusN(t *testing.T) {
	s := newRealSupervisor(t)
	mustConfigureStart(t, s, specWith("/bin/sh", "-c", "kill -9 $$"))
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.LastExit == nil {
		t.Fatal("no LastExit")
	}
	if st.LastExit.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137 (128+SIGKILL)", st.LastExit.ExitCode)
	}
	if st.LastExit.Signal != "SIGKILL" {
		t.Errorf("Signal = %q, want SIGKILL", st.LastExit.Signal)
	}
}

// startTrapService starts a sh script of the form "trap ...; ready;
// loop" and blocks until the trap is installed (signalled through a
// ready file), so the test's stop can't race the shell's startup.
func startTrapService(t *testing.T, s *supervisor, trap string) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	script := trap + `; : > "$CRUCIBLE_TEST_READY"; while :; do sleep 0.05; done`
	spec := &agentwire.ServiceSpec{
		Cmd: []string{"/bin/sh", "-c", script},
		Env: map[string]string{"CRUCIBLE_TEST_READY": ready},
	}
	mustConfigureStart(t, s, spec)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("service never signalled readiness")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestServiceRealGracefulStop(t *testing.T) {
	s := newRealSupervisor(t)
	startTrapService(t, s, `trap 'exit 0' TERM`)
	if _, err := s.Stop(0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if !st.LastExitRequested {
		t.Error("stop exit not marked requested")
	}
	if st.LastExit == nil || st.LastExit.ExitCode != 0 {
		t.Errorf("LastExit = %+v, want clean trap exit", st.LastExit)
	}
}

func TestServiceRealGraceEscalationKillsGroup(t *testing.T) {
	s := newRealSupervisor(t)
	startTrapService(t, s, `trap '' TERM`)
	if _, err := s.Stop(100 * time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := waitForState(t, s, agentwire.ServiceStateStopped)
	if st.LastExit == nil || st.LastExit.Signal != "SIGKILL" {
		t.Errorf("LastExit = %+v, want SIGKILL death", st.LastExit)
	}
	if st.LastExit.OomKilled {
		t.Error("our own SIGKILL misreported as OOM")
	}
	if !st.LastExitRequested {
		t.Error("escalated stop not marked requested")
	}
}

func TestServiceRealEnvAndCwd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	s := newRealSupervisor(t)
	spec := &agentwire.ServiceSpec{
		Cmd: []string{"/bin/sh", "-c", `printf '%s %s' "$CRUCIBLE_TEST_FOO" "$PWD" > out`},
		Env: map[string]string{"CRUCIBLE_TEST_FOO": "bar"},
		Cwd: dir,
	}
	mustConfigureStart(t, s, spec)
	waitForState(t, s, agentwire.ServiceStateStopped)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if got, want := string(data), "bar "+dir; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestServiceRealStatusReportsLiveRSS(t *testing.T) {
	s := newRealSupervisor(t)
	mustConfigureStart(t, s, specWith("/bin/sh", "-c", "sleep 60"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := s.Status()
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.LiveRSSBytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LiveRSSBytes never became positive for a live process")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Stop(time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, s, agentwire.ServiceStateStopped)
}
