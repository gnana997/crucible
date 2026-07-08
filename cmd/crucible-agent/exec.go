//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxExecRequestBody bounds the POST /exec request body. Exec requests
// are JSON — a cmd, env, cwd, timeout — nothing large.
const maxExecRequestBody = 1 << 20 // 1 MiB

// execWaitDelay bounds how long cmd.Wait blocks after the process exits or
// its context is cancelled. A backgrounded guest child can inherit the
// command's stdout/stderr pipe and hold it open long after the parent exits;
// without this grace Wait would block on that pipe until the child closes it
// (potentially never), wedging the handler + pollIO goroutines and the vsock
// conn. After the grace, Go unblocks the pipe reads and returns.
const execWaitDelay = 2 * time.Second

// handleExec runs a command inside the guest and streams the result
// back as a sequence of agentwire frames.
//
// Response flow:
//  1. Parse ExecRequest JSON (<=1 MiB).
//  2. Start command; pipe stdout → FrameStdout frames, stderr → FrameStderr.
//  3. Wait for the command to exit (or its deadline to fire).
//  4. Write a terminal FrameExit frame whose payload is a JSON ExecResult.
//  5. Return.
//
// The handler tries to always deliver a FrameExit so the host can tell
// the difference between "command finished" and "connection died" —
// even on early errors, we try to write an exit frame with Error set.
func handleExec(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxExecRequestBody)
	var req agentwire.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Headers not sent yet; return a plain 400.
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Cmd) == 0 {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return
	}

	// From this point on, we commit to a 200 streamed response. Any
	// failure beyond this gets reported as a FrameExit with Error set.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	// Flush frames as they're written so the host sees output live
	// rather than everything at command exit.
	flusher, _ := w.(http.Flusher)
	fw := agentwire.NewFrameWriter(flushOnWrite{w: w, flusher: flusher})

	// Build command context. A zero TimeoutSec means "inherit request
	// context only"; otherwise enforce a hard deadline on top.
	cmdCtx, cancel := commandContext(r.Context(), req.TimeoutSec)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Env = buildEnv(req.Env)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Stdout = fw.Stream(agentwire.FrameStdout)
	cmd.Stderr = fw.Stream(agentwire.FrameStderr)
	// Put the command in its own process group and wire a cancel that
	// SIGKILLs the whole group on timeout/disconnect, plus a WaitDelay
	// backstop so an inherited pipe can't wedge Wait. See configureExecProcess.
	configureExecProcess(cmd)

	// Use Start + Wait rather than Run so we can run a /proc/<pid>/io
	// poller alongside the child. Start failures (ENOENT, EACCES,
	// etc.) skip straight to the error path — no child, no stats.
	if err := cmd.Start(); err != nil {
		result := resultFromError(err, cmd, cmdCtx.Err(), time.Since(start))
		writeExitFrame(fw, result)
		return
	}

	// Spawn the I/O poller. last holds the most recent successful
	// /proc/<pid>/io snapshot; stop closes when cmd.Wait returns so
	// the goroutine exits promptly; done signals the goroutine has
	// finished so we can safely read last.
	var lastIO atomic.Pointer[procIOStats]
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go pollIO(cmd.Process.Pid, &lastIO, stopPoll, pollDone)

	runErr := cmd.Wait()
	close(stopPoll)
	<-pollDone

	result := resultFromError(runErr, cmd, cmdCtx.Err(), time.Since(start))
	attachUsage(&result, cmd.ProcessState, lastIO.Load(), cmdCtx.Err())

	writeExitFrame(fw, result)

	slog.Info("exec completed",
		"cmd", req.Cmd,
		"exit_code", result.ExitCode,
		"duration_ms", result.DurationMs,
		"timed_out", result.TimedOut,
		"oom_killed", result.OomKilled,
	)
}

// attachUsage populates result.Usage and result.OomKilled from the
// child's Rusage + the final /proc/<pid>/io snapshot. Safe to call
// with a nil ProcessState (it just becomes a no-op), for parity
// with start-failure paths.
func attachUsage(result *agentwire.ExecResult, ps *os.ProcessState, ioStats *procIOStats, ctxErr error) {
	if ps == nil {
		return
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return
	}
	result.Usage = buildUsage(ru, ioStats)
	// ctxErr != nil means the ctx was done (deadline OR client cancel) and we
	// SIGKILLed the group — so it's our kill, not an OOM. TimedOut alone
	// wouldn't catch a client cancel (context.Canceled), which is why we key
	// on the raw ctx error here.
	ws, _ := ps.Sys().(syscall.WaitStatus)
	result.OomKilled = detectOOM(ws, ctxErr != nil, result.Usage.PeakMemoryBytes, guestMemTotalBytes())
}

// buildEnv composes the command's environment. The agent's own env is
// the floor; request env overrides and adds. Keeping PATH/HOME/TERM
// from the agent means common tools (python3, /bin/sh) resolve without
// callers needing to re-specify PATH on every exec.
//
// When neither the agent's environ nor the request sets PATH — the case
// for a PID-1 agent, whose kernel-provided env has none — a default is
// added so bare commands resolve consistently regardless of the guest
// shell's compiled-in default (busybox's excludes /usr/local/bin, so
// `docker exec`-style tools there would otherwise not be found). Matches
// what `docker exec` supplies.
func buildEnv(override map[string]string) []string {
	return ensureDefaultPath(mergeEnv(os.Environ(), override))
}

// ensureDefaultPath appends PATH=dockerDefaultPath to env when it has no
// PATH entry. A no-op when PATH is already present (profile agents,
// which inherit systemd's PATH, and any request that sets one).
func ensureDefaultPath(env []string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			return env
		}
	}
	return append(env, "PATH="+dockerDefaultPath)
}

// mergeEnv is buildEnv's testable core: take a base slice of KEY=VAL
// lines and a map of overrides, return a new slice with overrides
// applied. Split out so tests don't depend on os.Environ.
func mergeEnv(base []string, override map[string]string) []string {
	merged := make(map[string]string, len(base)+len(override))
	for _, kv := range base {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				merged[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range override {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// commandContext derives a context from the HTTP request with an
// optional TimeoutSec deadline on top. Returning the cancel func lets
// the caller free resources deterministically.
func commandContext(parent context.Context, timeoutSec int) (context.Context, context.CancelFunc) {
	if timeoutSec <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(timeoutSec)*time.Second)
}

// configureExecProcess wires the timeout-kill and pipe-drain discipline onto
// cmd. It must be called before cmd.Start (Cancel/WaitDelay are read at Start).
//
//   - Setpgid puts the command in its own process group so a single kill can
//     reach the command *and* every child it spawns.
//   - Cancel fires when the command's context is cancelled (timeout or client
//     disconnect). exec.CommandContext's default cancel is Process.Kill(),
//     which — with a positive pid — signals only the group *leader*, leaving
//     backgrounded children alive and defeating the timeout. We SIGKILL the
//     negative pid (the whole group) instead. ESRCH means the group already
//     exited, which we report as ErrProcessDone (a success for cancel).
//   - WaitDelay bounds Wait after exit/cancel so a child that inherited our
//     stdout/stderr pipe can't keep Wait blocked forever.
func configureExecProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid = the whole process group; Setpgid made the group id
		// equal the leader's pid.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = execWaitDelay
}

// resultFromError inspects cmd.Run's error and the context's error to
// populate an ExecResult faithfully.
func resultFromError(runErr error, cmd *exec.Cmd, ctxErr error, elapsed time.Duration) agentwire.ExecResult {
	r := agentwire.ExecResult{DurationMs: elapsed.Milliseconds()}

	// Happy path.
	if runErr == nil {
		r.ExitCode = 0
		return r
	}

	// Context-caused failure → timed_out.
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		r.ExitCode = -1
		r.Signal = "SIGKILL"
		r.TimedOut = true
		return r
	}

	// Command started but exited non-zero or by signal.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		ps := exitErr.ProcessState
		r.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Signal = ws.Signal().String()
		}
		return r
	}

	// Command could not start (e.g. no such file).
	r.ExitCode = -1
	r.Error = runErr.Error()
	return r
}

// writeExitFrame is a best-effort terminal frame write. If it fails,
// the host sees a truncated stream — same outcome as a crashed agent.
func writeExitFrame(fw *agentwire.FrameWriter, result agentwire.ExecResult) {
	payload, err := json.Marshal(result)
	if err != nil {
		// Degenerate fallback; ExecResult only contains string/int, so
		// Marshal shouldn't fail in practice.
		payload = []byte(fmt.Sprintf(`{"exit_code":-1,"error":%q}`, err.Error()))
	}
	_ = fw.WriteFrame(agentwire.FrameExit, payload)
}

// flushOnWrite wraps an http.ResponseWriter so every Write immediately
// flushes the underlying chunked response. Without this the stdlib
// would buffer the body and the host wouldn't see stdout until command
// exit — defeating the point of streaming.
type flushOnWrite struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (f flushOnWrite) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil && f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}
