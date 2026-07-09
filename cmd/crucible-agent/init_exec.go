//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// handleExecInit is the /exec handler when the agent runs as PID 1.
// It mirrors handleExec's framing contract exactly (a committed 200
// followed by stdout/stderr frames and a terminal exit frame), but
// spawns through the reaper instead of os/exec — because in init mode
// no code may call os/exec Wait (it would race the reaper's wait4).
func (r *reaper) handleExecInit(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("stdin") == "1" {
		r.handleExecInitInteractive(w, req)
		return
	}

	start := time.Now()

	req.Body = http.MaxBytesReader(w, req.Body, maxExecRequestBody)
	var er agentwire.ExecRequest
	if err := json.NewDecoder(req.Body).Decode(&er); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	if len(er.Cmd) == 0 {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fw := agentwire.NewFrameWriter(flushOnWrite{w: w, flusher: flusher})

	ctx, cancel := commandContext(req.Context(), er.TimeoutSec)
	defer cancel()

	// One-shot exec keeps today's semantics (agent env, root); Docker-
	// exec-style per-user/env fidelity is a later refinement.
	rp, err := r.spawn(er.Cmd, buildEnv(er.Env), er.Cwd, nil, nil,
		fw.Stream(agentwire.FrameStdout), fw.Stream(agentwire.FrameStderr))
	if err != nil {
		writeExitFrame(fw, agentwire.ExecResult{
			ExitCode:   -1,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	// Kill the whole group when the deadline fires or the client
	// disconnects; stop the watcher once the process is reaped.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rp.signal(syscall.SIGKILL)
		case <-done:
		}
	}()

	// Poll /proc/<pid>/io alongside the child, like handleExec.
	var lastIO atomic.Pointer[procIOStats]
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go pollIO(rp.pid, &lastIO, stopPoll, pollDone)

	res := rp.wait()
	close(done)
	close(stopPoll)
	<-pollDone

	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	result := execResultFromWait(res, lastIO.Load(), time.Since(start), timedOut, ctx.Err() != nil)
	writeExitFrame(fw, result)

	slog.Info("exec completed (init)",
		"cmd", er.Cmd,
		"exit_code", result.ExitCode,
		"duration_ms", result.DurationMs,
		"timed_out", result.TimedOut,
		"oom_killed", result.OomKilled,
	)
}

// execResultFromWait builds an ExecResult from raw wait4 results,
// matching handleExec's conventions so /exec behaves identically in
// both boot modes: a timeout reports ExitCode -1 + TimedOut, a signal
// death reports ExitCode -1 + the Go signal name, a normal exit reports
// its code.
func execResultFromWait(res waitResult, io *procIOStats, elapsed time.Duration, timedOut, killedByCtx bool) agentwire.ExecResult {
	r := agentwire.ExecResult{DurationMs: elapsed.Milliseconds()}
	switch {
	case timedOut:
		r.ExitCode = -1
		r.Signal = "SIGKILL"
		r.TimedOut = true
	case res.ws.Signaled():
		r.ExitCode = -1
		r.Signal = res.ws.Signal().String()
	default:
		r.ExitCode = res.ws.ExitStatus()
	}
	r.Usage = buildUsage(&res.rusage, io)
	r.OomKilled = detectOOM(res.ws, killedByCtx, r.Usage.PeakMemoryBytes, guestMemTotalBytes())
	return r
}
