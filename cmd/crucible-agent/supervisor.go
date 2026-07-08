//go:build linux

package main

import (
	"errors"
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/agentwire"
)

// errNoServiceSpec is returned by start/restart before any spec has
// been configured. Handlers map it to 409.
var errNoServiceSpec = errors.New("no service configured")

// errSupervisorDown is returned when the supervisor has shut down.
var errSupervisorDown = errors.New("supervisor shut down")

// Restart backoff, mirroring Docker's numbers: delays start at 100ms
// and double per consecutive restart up to a 1-minute cap; a run that
// survives 10 seconds resets both the delay and the on-failure retry
// budget.
const (
	restartBackoffInitial = 100 * time.Millisecond
	restartBackoffMax     = time.Minute
	restartStableAfter    = 10 * time.Second
)

type svcCmdKind int

const (
	svcConfigure svcCmdKind = iota
	svcStart
	svcStop
	svcRestart
	svcStatus
	svcShutdown
)

type svcCommand struct {
	kind  svcCmdKind
	spec  *agentwire.ServiceSpec // configure only
	grace time.Duration          // stop only; 0 = use the spec's grace
	reply chan svcReply
}

type svcReply struct {
	status agentwire.ServiceStatus
	err    error
}

// stopReason records why a stop was initiated. It decides both
// LastExitRequested and what happens once the process exits.
type stopReason int

const (
	stopNone      stopReason = iota // process exited on its own
	stopRequested                   // explicit stop command (or shutdown)
	stopRestart                     // explicit restart command
	stopRespec                      // configure replaced the spec while running
)

// supervisor owns the one supervised service of this sandbox. All state
// lives in the loop goroutine; the exported methods are thin
// synchronous wrappers that post a command and wait for the reply, so
// the state machine is serialized by construction.
type supervisor struct {
	runner      childRunner
	clk         clock
	log         *slog.Logger
	pidfilePath string // "" disables pidfile writing (unit tests)

	cmds chan svcCommand
	done chan struct{} // closed when the loop exits (after shutdown)

	// ring is the log capture for the currently-installed spec. The
	// loop swaps it when a spec is installed; HTTP readers load it
	// without going through the loop (the ring locks internally). It
	// deliberately survives restarts of the same spec — logs from the
	// previous run stay readable — and is replaced on re-spec.
	ring atomic.Pointer[logRing]
}

// svcState is the loop-owned state. Nothing outside the loop goroutine
// may touch it.
type svcState struct {
	spec    *agentwire.ServiceSpec
	stopSig syscall.Signal

	state  string
	child  serviceChild
	exitCh chan childExit

	// stopping bookkeeping
	reason     stopReason
	graceCh    <-chan time.Time
	killedByUs bool

	// restart-policy bookkeeping
	backoffCh  <-chan time.Time // armed while backing_off
	backoffCur time.Duration    // next delay; 0 means restartBackoffInitial
	retries    int              // consecutive policy restarts (on-failure budget)

	// deferred work applied when the current process exits
	pendingStart bool
	pendingSpec  *agentwire.ServiceSpec
	pendingSig   syscall.Signal

	startedAt         time.Time
	restarts          int
	lastExit          *agentwire.ExecResult
	lastExitRequested bool
	lastExitAt        time.Time

	shutdownReply chan svcReply // non-nil once shutdown was requested
	terminate     bool
}

func newSupervisor(runner childRunner, clk clock, log *slog.Logger, pidfilePath string) *supervisor {
	s := &supervisor{
		runner:      runner,
		clk:         clk,
		log:         log,
		pidfilePath: pidfilePath,
		cmds:        make(chan svcCommand),
		done:        make(chan struct{}),
	}
	go s.loop()
	return s
}

// Configure validates, normalizes, and installs a spec. If a process is
// running it is stopped (with its old grace) and relaunched under the
// new spec; in any other state the spec is swapped in place and the
// service is left stopped for an explicit start.
func (s *supervisor) Configure(spec *agentwire.ServiceSpec) (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcConfigure, spec: spec})
}

// Start launches the configured service. No-op if already running.
func (s *supervisor) Start() (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcStart})
}

// Stop stops the service: StopSignal to the process group, grace, then
// SIGKILL. grace == 0 means the spec's StopGraceSec. No-op if not
// running.
func (s *supervisor) Stop(grace time.Duration) (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcStop, grace: grace})
}

// Restart stops the service (if running) and starts it again.
func (s *supervisor) Restart() (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcRestart})
}

// Status reports the current state. Never errors while the supervisor
// is alive.
func (s *supervisor) Status() (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcStatus})
}

// Shutdown stops the service (spec grace) and terminates the loop.
// Blocks until done. Further calls on the supervisor error.
func (s *supervisor) Shutdown() (agentwire.ServiceStatus, error) {
	return s.do(svcCommand{kind: svcShutdown})
}

func (s *supervisor) do(cmd svcCommand) (agentwire.ServiceStatus, error) {
	cmd.reply = make(chan svcReply, 1)
	select {
	case s.cmds <- cmd:
	case <-s.done:
		return agentwire.ServiceStatus{}, errSupervisorDown
	}
	select {
	case r := <-cmd.reply:
		return r.status, r.err
	case <-s.done:
		// The loop replies (buffered) and then exits on shutdown, so
		// both channels can be ready at once and select picks randomly.
		// Prefer a reply that made it out over reporting the shutdown.
		select {
		case r := <-cmd.reply:
			return r.status, r.err
		default:
			return agentwire.ServiceStatus{}, errSupervisorDown
		}
	}
}

func (s *supervisor) loop() {
	defer close(s.done)
	st := &svcState{state: agentwire.ServiceStateIdle}
	for {
		select {
		case cmd := <-s.cmds:
			s.handleCommand(st, cmd)
		case exit := <-st.exitCh:
			s.onChildExit(st, exit)
		case <-st.graceCh:
			s.onGraceExpired(st)
		case <-st.backoffCh:
			s.onBackoffFired(st)
		}
		if st.terminate {
			return
		}
	}
}

func (s *supervisor) handleCommand(st *svcState, cmd svcCommand) {
	switch cmd.kind {
	case svcConfigure:
		s.cmdConfigure(st, cmd)
	case svcStart:
		s.cmdStart(st, cmd)
	case svcStop:
		s.cmdStop(st, cmd)
	case svcRestart:
		s.cmdRestart(st, cmd)
	case svcStatus:
		s.reply(st, cmd, nil)
	case svcShutdown:
		s.cmdShutdown(st, cmd)
	}
}

func (s *supervisor) reply(st *svcState, cmd svcCommand, err error) {
	cmd.reply <- svcReply{status: s.buildStatus(st), err: err}
}

func (s *supervisor) cmdConfigure(st *svcState, cmd svcCommand) {
	spec := cmd.spec
	if err := spec.Validate(); err != nil {
		s.reply(st, cmd, err)
		return
	}
	spec.Normalize()
	sig, err := resolveSignal(spec.StopSignal)
	if err != nil {
		s.reply(st, cmd, err)
		return
	}

	switch st.state {
	case agentwire.ServiceStateRunning, agentwire.ServiceStateStarting:
		// Re-spec while running: stop under the old spec's grace, swap
		// and relaunch when the old process is gone.
		st.pendingSpec, st.pendingSig = spec, sig
		s.beginStop(st, stopRespec, s.specGrace(st))
	case agentwire.ServiceStateStopping:
		// A stop is already in flight; swap the spec once it lands.
		// Whether the service comes back up is decided by the reason
		// that stop was started with (restart/respec yes, stop no).
		st.pendingSpec, st.pendingSig = spec, sig
	default:
		// Includes backing_off and failed: a new spec cancels any
		// scheduled policy relaunch and waits for an explicit start.
		cancelBackoff(st)
		resetPolicy(st)
		s.installSpec(st, spec, sig)
		st.restarts = 0
		st.state = agentwire.ServiceStateStopped
	}
	s.reply(st, cmd, nil)
}

// cancelBackoff clears a scheduled policy relaunch. Explicit commands
// always win over the restart policy.
func cancelBackoff(st *svcState) {
	st.backoffCh = nil
}

// resetPolicy returns the backoff delay and on-failure retry budget to
// their initial values.
func resetPolicy(st *svcState) {
	st.backoffCur = 0
	st.retries = 0
}

// installSpec makes spec the active one and gives it a fresh log ring.
// Restarts under an unchanged spec keep their ring (history spans
// process restarts); a new spec starts clean.
func (s *supervisor) installSpec(st *svcState, spec *agentwire.ServiceSpec, sig syscall.Signal) {
	st.spec, st.stopSig = spec, sig
	s.ring.Store(newLogRing(spec.LogBufferBytes))
}

func (s *supervisor) cmdStart(st *svcState, cmd svcCommand) {
	if st.spec == nil && st.pendingSpec == nil {
		s.reply(st, cmd, errNoServiceSpec)
		return
	}
	switch st.state {
	case agentwire.ServiceStateRunning, agentwire.ServiceStateStarting:
		// Idempotent: already running is success, so a caller (or a
		// future reconciler) can retry blindly.
	case agentwire.ServiceStateStopping:
		st.pendingStart = true
	default:
		// Includes backing_off (the relaunch happens now, not after
		// the delay) and failed (the host may retry).
		cancelBackoff(st)
		resetPolicy(st)
		st.restarts = 0
		s.launch(st)
	}
	s.reply(st, cmd, nil)
}

func (s *supervisor) cmdStop(st *svcState, cmd svcCommand) {
	switch st.state {
	case agentwire.ServiceStateRunning, agentwire.ServiceStateStarting:
		grace := cmd.grace
		if grace <= 0 {
			grace = s.specGrace(st)
		}
		s.beginStop(st, stopRequested, grace)
	case agentwire.ServiceStateStopping:
		// Already stopping: an explicit stop cancels any pending
		// relaunch (a stop must win over a restart in flight).
		st.pendingStart = false
		st.reason = stopRequested
	case agentwire.ServiceStateBackingOff:
		// Stop cancels the scheduled policy relaunch.
		cancelBackoff(st)
		resetPolicy(st)
		st.state = agentwire.ServiceStateStopped
	default:
		// Idempotent: not running is what the caller asked for.
	}
	s.reply(st, cmd, nil)
}

func (s *supervisor) cmdRestart(st *svcState, cmd svcCommand) {
	if st.spec == nil && st.pendingSpec == nil {
		s.reply(st, cmd, errNoServiceSpec)
		return
	}
	switch st.state {
	case agentwire.ServiceStateRunning, agentwire.ServiceStateStarting:
		s.beginStop(st, stopRestart, s.specGrace(st))
	case agentwire.ServiceStateStopping:
		st.pendingStart = true
	default:
		cancelBackoff(st)
		resetPolicy(st)
		st.restarts = 0
		s.launch(st)
	}
	s.reply(st, cmd, nil)
}

func (s *supervisor) cmdShutdown(st *svcState, cmd svcCommand) {
	if st.child == nil {
		st.state = agentwire.ServiceStateStopped
		if st.spec == nil {
			st.state = agentwire.ServiceStateIdle
		}
		st.terminate = true
		s.reply(st, cmd, nil)
		return
	}
	st.shutdownReply = cmd.reply
	st.pendingStart = false
	if st.state != agentwire.ServiceStateStopping {
		s.beginStop(st, stopRequested, s.specGrace(st))
	}
}

func (s *supervisor) specGrace(st *svcState) time.Duration {
	if st.spec == nil {
		return time.Duration(agentwire.DefaultStopGraceSec) * time.Second
	}
	return time.Duration(st.spec.StopGraceSec) * time.Second
}

// launch starts the configured entrypoint. On start failure the service
// goes to failed — there is no process, so nothing else can happen
// until the host intervenes.
func (s *supervisor) launch(st *svcState) {
	st.state = agentwire.ServiceStateStarting
	ring := s.ring.Load()
	if ring == nil {
		// Unreachable — installSpec always precedes launch — but a nil
		// ring must never reach the writer goroutines.
		ring = newLogRing(st.spec.LogBufferBytes)
		s.ring.Store(ring)
	}
	stdout := ringWriter{ring: ring, stream: agentwire.ServiceLogStdout, now: s.clk.Now}
	stderr := ringWriter{ring: ring, stream: agentwire.ServiceLogStderr, now: s.clk.Now}
	child, err := s.runner.start(st.spec, stdout, stderr)
	if err != nil {
		st.lastExit = &agentwire.ExecResult{ExitCode: -1, Error: err.Error()}
		st.lastExitRequested = false
		st.lastExitAt = s.clk.Now()
		s.log.Error("service start failed", "cmd", st.spec.Cmd, "err", err)
		// The restart policy applies to start failures too (Docker
		// semantics); with no policy the service lands in failed.
		s.applyRestartPolicy(st, true)
		return
	}
	st.child = child
	st.startedAt = s.clk.Now()
	st.reason = stopNone
	st.killedByUs = false
	st.graceCh = nil
	st.exitCh = make(chan childExit, 1)
	st.state = agentwire.ServiceStateRunning
	if s.pidfilePath != "" {
		if err := writeServicePidFile(s.pidfilePath, child.pid()); err != nil {
			s.log.Warn("write service pidfile failed", "err", err)
		}
	}
	go func(c serviceChild, ch chan<- childExit) {
		ch <- c.wait()
	}(child, st.exitCh)
	s.log.Info("service started", "cmd", st.spec.Cmd, "pid", child.pid())
}

func (s *supervisor) beginStop(st *svcState, reason stopReason, grace time.Duration) {
	st.reason = reason
	st.state = agentwire.ServiceStateStopping
	if err := st.child.signalGroup(st.stopSig); err != nil {
		// A real signalling failure (not ESRCH). The grace timer still
		// runs; SIGKILL follows if the process doesn't exit.
		s.log.Warn("service stop signal failed", "signal", unix.SignalName(st.stopSig), "err", err)
	}
	st.graceCh = s.clk.After(grace)
	s.log.Info("service stopping", "signal", unix.SignalName(st.stopSig), "grace", grace)
}

// onGraceExpired escalates a stop to SIGKILL. If the exit is already in
// flight (both channels ready, select picked this one) the kill is a
// harmless ESRCH no-op; killedByUs may then be set on a process that
// died on its own inside the grace window, which only suppresses the
// (already inapplicable) OOM heuristic for that exit.
func (s *supervisor) onGraceExpired(st *svcState) {
	if st.state != agentwire.ServiceStateStopping || st.child == nil {
		return
	}
	st.killedByUs = true
	st.graceCh = nil
	_ = st.child.signalGroup(syscall.SIGKILL)
	s.log.Warn("service stop grace expired, killed process group")
}

func (s *supervisor) onChildExit(st *svcState, exit childExit) {
	if s.pidfilePath != "" {
		removeServicePidFile(s.pidfilePath)
	}
	res := serviceResult(exit, st.killedByUs)
	st.lastExit = &res
	st.lastExitRequested = st.reason != stopNone
	st.lastExitAt = s.clk.Now()

	reason := st.reason
	st.child = nil
	st.exitCh = nil
	st.graceCh = nil
	st.reason = stopNone
	st.killedByUs = false

	s.log.Info("service exited",
		"exit_code", res.ExitCode,
		"signal", res.Signal,
		"requested", st.lastExitRequested,
		"duration_ms", res.DurationMs,
	)

	if st.shutdownReply != nil {
		st.state = agentwire.ServiceStateStopped
		st.terminate = true
		st.shutdownReply <- svcReply{status: s.buildStatus(st)}
		return
	}

	if st.pendingSpec != nil {
		s.installSpec(st, st.pendingSpec, st.pendingSig)
		st.pendingSpec = nil
		st.restarts = 0
	}

	switch {
	case st.pendingStart || reason == stopRestart || reason == stopRespec:
		st.pendingStart = false
		st.restarts = 0
		resetPolicy(st)
		s.launch(st)
	case reason == stopRequested:
		st.state = agentwire.ServiceStateStopped
		resetPolicy(st)
	default:
		// The process exited on its own: the restart policy decides.
		s.applyRestartPolicy(st, false)
	}
}

// applyRestartPolicy decides what happens after a self-exit (or, with
// startFailure, after a launch that never produced a process).
// Semantics mirror Docker: always restarts any exit; on-failure
// restarts non-zero exits up to MaxRetries consecutive failures; a run
// that survived restartStableAfter resets the delay and the budget.
func (s *supervisor) applyRestartPolicy(st *svcState, startFailure bool) {
	if !startFailure && st.lastExit != nil && st.lastExit.DurationMs >= restartStableAfter.Milliseconds() {
		resetPolicy(st)
	}

	idleState := agentwire.ServiceStateStopped
	if startFailure {
		// No process ever existed; "stopped" would misreport.
		idleState = agentwire.ServiceStateFailed
	}

	policy := st.spec.Restart
	failure := st.lastExit == nil || st.lastExit.ExitCode != 0
	var restart bool
	switch policy.Policy {
	case agentwire.RestartAlways:
		restart = true
	case agentwire.RestartOnFailure:
		restart = failure
	}
	if !restart {
		st.state = idleState
		return
	}
	if policy.Policy == agentwire.RestartOnFailure && policy.MaxRetries > 0 && st.retries >= policy.MaxRetries {
		st.state = agentwire.ServiceStateFailed
		s.log.Error("service restart budget exhausted", "retries", st.retries, "max_retries", policy.MaxRetries)
		return
	}

	delay := st.backoffCur
	if delay == 0 {
		delay = restartBackoffInitial
	}
	st.backoffCur = min(2*delay, restartBackoffMax)
	st.retries++
	st.state = agentwire.ServiceStateBackingOff
	st.backoffCh = s.clk.After(delay)
	s.log.Info("service restart scheduled", "delay", delay, "consecutive_retries", st.retries)
}

// onBackoffFired performs the policy relaunch scheduled by
// applyRestartPolicy. A stale timer (state moved on via an explicit
// command) is ignored.
func (s *supervisor) onBackoffFired(st *svcState) {
	if st.state != agentwire.ServiceStateBackingOff {
		return
	}
	st.backoffCh = nil
	st.restarts++
	s.launch(st)
}

func (s *supervisor) buildStatus(st *svcState) agentwire.ServiceStatus {
	out := agentwire.ServiceStatus{
		State:    st.state,
		Spec:     st.spec,
		Restarts: st.restarts,
	}
	if st.child != nil {
		out.Pid = st.child.pid()
		out.StartedAtUnixMs = st.startedAt.UnixMilli()
		out.UptimeMs = s.clk.Now().Sub(st.startedAt).Milliseconds()
		out.LiveRSSBytes, out.LivePeakRSSBytes = readProcRSS(out.Pid)
	}
	if st.lastExit != nil {
		out.LastExit = st.lastExit
		out.LastExitRequested = st.lastExitRequested
		out.LastExitAtUnixMs = st.lastExitAt.UnixMilli()
	}
	if ring := s.ring.Load(); ring != nil {
		out.LogFirstSeq, out.LogNextSeq, out.LogDroppedBytes = ring.stats()
	}
	return out
}

// Logs reads captured output from the log ring. Reads bypass the loop —
// the ring synchronizes internally — so a busy supervisor never delays
// a log tail.
func (s *supervisor) Logs(fromSeq uint64, maxBytes int) (agentwire.ServiceLogsResponse, error) {
	ring := s.ring.Load()
	if ring == nil {
		return agentwire.ServiceLogsResponse{}, errNoServiceSpec
	}
	return ring.read(fromSeq, maxBytes), nil
}

// serviceResult turns raw wait results into the wire ExecResult. Unlike
// exec's resultFromError, signal deaths report ExitCode 128+signal (the
// Docker convention — a supervised service's exit code is part of the
// operator contract) alongside the SIGNAME-style signal name.
func serviceResult(exit childExit, killedByUs bool) agentwire.ExecResult {
	r := agentwire.ExecResult{DurationMs: exit.elapsed.Milliseconds()}

	switch {
	case exit.startErr != nil:
		r.ExitCode = -1
		r.Error = exit.startErr.Error()
	case exit.ws.Signaled():
		r.Signal = unix.SignalName(exit.ws.Signal())
		r.ExitCode = 128 + int(exit.ws.Signal())
	default:
		r.ExitCode = exit.ws.ExitStatus()
	}

	if exit.rusage != nil {
		r.Usage = buildUsage(exit.rusage, exit.io)
		// killedByUs (grace escalation) means our SIGKILL, not an OOM.
		r.OomKilled = detectOOM(exit.ws, killedByUs, r.Usage.PeakMemoryBytes, guestMemTotalBytes())
	}
	return r
}
