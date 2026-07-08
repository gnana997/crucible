//go:build linux

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// reaper is the single owner of wait4 when the agent runs as PID 1.
//
// As init, the kernel reparents every orphaned process to us and
// delivers SIGCHLD when any child dies; PID 1 must reap them or they
// zombie forever. But a naive wait4(-1) loop steals exit statuses from
// os/exec's own Wait (the documented go-reaper race), so in init mode
// NOTHING may call os/exec Wait: every process is spawned through the
// reaper, which owns all waiting and dispatches each child's status to
// whoever registered it. Unregistered children (true orphans) are
// reaped and discarded.
//
// The reaper is created and started only in init mode. In profile mode
// systemd is PID 1 and reaps orphans, so os/exec Wait per child is
// correct and no reaper exists.
type reaper struct {
	log *slog.Logger

	mu      sync.Mutex
	waiters map[int]chan waitResult // pid -> its awaiting spawner
	pending map[int]waitResult      // reaped before the spawner registered
	closed  bool

	sigCh chan os.Signal
}

type waitResult struct {
	ws     syscall.WaitStatus
	rusage syscall.Rusage
}

func newReaper(log *slog.Logger) *reaper {
	return &reaper{
		log:     log,
		waiters: make(map[int]chan waitResult),
		pending: make(map[int]waitResult),
		sigCh:   make(chan os.Signal, 1),
	}
}

// start begins reaping. It reaps once immediately (in case children
// already exited) and then on every SIGCHLD.
func (r *reaper) start() {
	signal.Notify(r.sigCh, syscall.SIGCHLD)
	go func() {
		r.reapAll()
		for range r.sigCh {
			r.reapAll()
		}
	}()
}

// stop ends signal delivery. Used by tests; the real init agent never
// stops reaping (it dies with the VM).
func (r *reaper) stop() {
	signal.Stop(r.sigCh)
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.sigCh)
	}
	r.mu.Unlock()
}

// reapAll drains every exited child. Held under mu so a spawn in
// progress (which also holds mu across StartProcess+register) can't
// have its child reaped into the void before it registers.
func (r *reaper) reapAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		var ru syscall.Rusage
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, &ru)
		if err == syscall.EINTR {
			continue
		}
		if pid <= 0 || err != nil {
			// pid 0: children exist but none exited. pid -1/ECHILD: no
			// children at all. Either way, nothing more to reap now.
			return
		}
		res := waitResult{ws: ws, rusage: ru}
		if ch, ok := r.waiters[pid]; ok {
			ch <- res // buffered(1); one result per pid
			delete(r.waiters, pid)
		} else {
			// Reaped before the spawner registered, or a true orphan.
			// Stash it; spawn checks pending right after StartProcess.
			r.pending[pid] = res
		}
	}
}

// spawn forks/execs a child and returns a handle whose wait() resolves
// via the reaper. Pipes are created for stdout/stderr and copied to the
// given writers by goroutines the handle owns. The child runs in its
// own process group (Setpgid) so one signal reaches it and every
// descendant.
//
// stdin is the child's fd 0. Pass nil for no stdin (the one-shot exec and
// supervised-service case); pass the read end of a pipe for interactive
// exec. On a successful start spawn closes stdin (the child owns its own
// dup), so the caller only holds the write end.
func (r *reaper) spawn(argv, env []string, dir string, cred *syscall.Credential, stdin *os.File, stdout, stderr io.Writer) (*reapedProc, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("reaper: empty argv")
	}
	// os.StartProcess does no PATH search, so a bare argv[0] (e.g. "sh"
	// from `exec` or the TUI's `sh -c`) would fail to start. Resolve it
	// against the child's PATH the way Docker does — matching profile
	// mode, where os/exec.Command already does this. An absolute path is
	// returned as-is, so the pre-resolved service argv is unaffected.
	exe, err := lookExecutable(argv[0], pathFromEnv(env), dir)
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	attr := &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []*os.File{stdin, outW, errW}, // stdin nil = no fd 0
		Sys:   &syscall.SysProcAttr{Setpgid: true, Credential: cred},
	}

	// Hold mu across StartProcess + registration so the reaper can't
	// dispatch this child before we know its pid.
	r.mu.Lock()
	proc, err := os.StartProcess(exe, argv, attr)
	if err != nil {
		r.mu.Unlock()
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, err
	}
	pid := proc.Pid
	ch := make(chan waitResult, 1)
	if res, ok := r.pending[pid]; ok {
		// Already reaped between StartProcess and here.
		ch <- res
		delete(r.pending, pid)
	} else {
		r.waiters[pid] = ch
	}
	r.mu.Unlock()

	// The write ends belong to the child now; close our copies so the
	// pipes report EOF when the child (and its descendants) let go. The
	// stdin read end likewise belongs to the child — close our copy so the
	// child sees EOF once the caller closes the write end.
	_ = outW.Close()
	_ = errW.Close()
	if stdin != nil {
		_ = stdin.Close()
	}

	rp := &reapedProc{
		proc:     proc,
		pid:      pid,
		waitCh:   ch,
		copyDone: make(chan struct{}),
	}
	go rp.copyStreams(outR, errR, stdout, stderr)
	return rp, nil
}

// reapedProc is a running child spawned by the reaper.
type reapedProc struct {
	proc     *os.Process
	pid      int
	waitCh   <-chan waitResult
	copyDone chan struct{}

	closeOnce sync.Once
	pipes     [2]*os.File
}

func (p *reapedProc) copyStreams(outR, errR *os.File, stdout, stderr io.Writer) {
	p.pipes = [2]*os.File{outR, errR}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(stdout, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(stderr, errR) }()
	wg.Wait()
	close(p.copyDone)
}

// signal sends sig to the whole process group. ESRCH (group already
// gone) is success from the caller's view — the exit is on its way.
func (p *reapedProc) signal(sig syscall.Signal) error {
	if err := syscall.Kill(-p.pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// wait blocks until the child is reaped, then drains its output —
// bounded by drainGrace so a descendant that inherited the pipes can't
// wedge us forever (the os/exec WaitDelay analogue).
func (p *reapedProc) wait() waitResult {
	res := <-p.waitCh
	select {
	case <-p.copyDone:
	case <-time.After(reapDrainGrace):
		// Force the copy goroutines to unblock by closing the read ends.
		p.closePipes()
		<-p.copyDone
	}
	return res
}

func (p *reapedProc) closePipes() {
	p.closeOnce.Do(func() {
		for _, f := range p.pipes {
			if f != nil {
				_ = f.Close()
			}
		}
	})
}

// reapDrainGrace mirrors execWaitDelay: how long to wait for stdout/
// stderr to drain after the process exits before force-closing the
// pipes.
const reapDrainGrace = 2 * time.Second

// initRunner is the childRunner used by the supervisor in init mode: it
// spawns through the reaper instead of os/exec, so the supervisor's
// service child is reaped by the same wait4 owner as everything else.
type initRunner struct {
	reaper *reaper
}

func (ir initRunner) start(spec *agentwire.ServiceSpec, stdout, stderr io.Writer) (serviceChild, error) {
	argv, env, cred, err := resolveLaunch(spec)
	if err != nil {
		return nil, err
	}
	rp, err := ir.reaper.spawn(argv, env, spec.Cwd, cred, nil, stdout, stderr)
	if err != nil {
		return nil, err
	}
	c := &initChild{
		rp:       rp,
		started:  time.Now(),
		stopPoll: make(chan struct{}),
		pollDone: make(chan struct{}),
	}
	go pollIO(rp.pid, &c.lastIO, c.stopPoll, c.pollDone)
	return c, nil
}

// initChild adapts a reapedProc to the serviceChild interface.
type initChild struct {
	rp       *reapedProc
	started  time.Time
	lastIO   atomic.Pointer[procIOStats]
	stopPoll chan struct{}
	pollDone chan struct{}
}

func (c *initChild) pid() int { return c.rp.pid }

func (c *initChild) signalGroup(sig syscall.Signal) error { return c.rp.signal(sig) }

func (c *initChild) wait() childExit {
	res := c.rp.wait()
	close(c.stopPoll)
	<-c.pollDone
	return childExit{
		ws:      res.ws,
		rusage:  &res.rusage,
		io:      c.lastIO.Load(),
		elapsed: time.Since(c.started),
	}
}
