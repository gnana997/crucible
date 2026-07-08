//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/agentwire"
)

// clock abstracts time for the supervisor so tests can drive the grace
// and backoff timers deterministically.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// resolveSignal maps a signal name ("SIGTERM") to its number, rejecting
// names this platform doesn't know. Done agent-side (not in agentwire)
// because it needs the platform signal table.
func resolveSignal(name string) (syscall.Signal, error) {
	sig := unix.SignalNum(name)
	if sig == 0 {
		return 0, fmt.Errorf("unknown signal %q", name)
	}
	return sig, nil
}

// childExit carries the raw wait results back to the supervisor loop.
// The loop — not the watcher — turns them into an ExecResult, because
// only the loop knows whether a SIGKILL was ours (grace escalation)
// and that distinction drives the OOM heuristic.
//
// It is deliberately runtime-agnostic: execChild fills it from os/exec's
// ProcessState, the init-mode child fills it from the janitor's raw
// wait4 results. ws/rusage are valid only when startErr is nil.
type childExit struct {
	ws       syscall.WaitStatus
	rusage   *syscall.Rusage
	io       *procIOStats
	elapsed  time.Duration
	startErr error // the process could not be started or reaped
}

// childExitFromState builds a childExit from os/exec's post-Wait state.
// After a successful Start, ProcessState is always populated (even on a
// non-zero or signalled exit), so a nil state means the wait itself
// failed.
func childExitFromState(waitErr error, ps *os.ProcessState, io *procIOStats, elapsed time.Duration) childExit {
	exit := childExit{io: io, elapsed: elapsed}
	if ps == nil {
		exit.startErr = waitErr
		return exit
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		exit.ws = ws
	}
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
		exit.rusage = ru
	}
	return exit
}

// serviceChild is one launched entrypoint process. wait blocks until
// the process is reaped and must be called exactly once.
type serviceChild interface {
	pid() int
	signalGroup(sig syscall.Signal) error
	wait() childExit
}

// childRunner launches entrypoint processes. The indirection exists for
// two reasons: unit tests substitute a fake, and the future PID-1 boot
// position (OCI images) substitutes a wait4-based janitor — an os/exec
// Wait per child is only correct while something else (systemd on
// profile rootfses) reaps orphans, and a global wait4(-1) loop next to
// os/exec would steal its exit statuses.
type childRunner interface {
	start(spec *agentwire.ServiceSpec, stdout, stderr io.Writer) (serviceChild, error)
}

// execRunner is the child-mode childRunner: plain os/exec with the same
// process-group and pipe-drain discipline as handleExec (see
// configureExecProcess for the rationale), minus the context — services
// have no deadline; stop is explicit.
type execRunner struct{}

type execChild struct {
	cmd      *exec.Cmd
	started  time.Time
	lastIO   atomic.Pointer[procIOStats]
	stopPoll chan struct{}
	pollDone chan struct{}
}

func (execRunner) start(spec *agentwire.ServiceSpec, stdout, stderr io.Writer) (serviceChild, error) {
	cmd := exec.Command(spec.Cmd[0], spec.Cmd[1:]...)
	cmd.Env = buildEnv(spec.Env)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Own process group so one signal reaches the entrypoint and every
	// descendant; WaitDelay so a grandchild that inherited our pipes
	// can't wedge wait() after the entrypoint is gone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = execWaitDelay

	c := &execChild{
		cmd:      cmd,
		started:  time.Now(),
		stopPoll: make(chan struct{}),
		pollDone: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go pollIO(cmd.Process.Pid, &c.lastIO, c.stopPoll, c.pollDone)
	return c, nil
}

func (c *execChild) pid() int { return c.cmd.Process.Pid }

// signalGroup signals the whole process group. ESRCH means the group is
// already gone — success from the caller's point of view (the exit is
// on its way through wait).
func (c *execChild) signalGroup(sig syscall.Signal) error {
	if err := syscall.Kill(-c.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (c *execChild) wait() childExit {
	runErr := c.cmd.Wait()
	close(c.stopPoll)
	<-c.pollDone
	return childExitFromState(runErr, c.cmd.ProcessState, c.lastIO.Load(), time.Since(c.started))
}

// readProcRSS reads current and peak resident set size for a live
// process from /proc/<pid>/status (VmRSS / VmHWM, reported in kB).
// Returns zeros on any error — status reporting is best-effort.
func readProcRSS(pid int) (rss, peak int64) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		var target *int64
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			target = &rss
		case strings.HasPrefix(line, "VmHWM:"):
			target = &peak
		default:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		*target = kb * 1024
	}
	return rss, peak
}
