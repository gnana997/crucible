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
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxExecRequestBody bounds the POST /exec request body. Exec requests
// are JSON — a cmd, env, cwd, timeout — nothing large.
const maxExecRequestBody = 1 << 20 // 1 MiB

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
	// Put the command in its own process group so we can SIGKILL the
	// whole group (the command + any children it spawns) on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	runErr := cmd.Run()

	result := resultFromError(runErr, cmd, cmdCtx.Err(), time.Since(start))
	writeExitFrame(fw, result)

	slog.Info("exec completed",
		"cmd", req.Cmd,
		"exit_code", result.ExitCode,
		"duration_ms", result.DurationMs,
		"timed_out", result.TimedOut,
	)
}

// buildEnv composes the command's environment. The agent's own env is
// the floor; request env overrides and adds. Keeping PATH/HOME/TERM
// from the agent means common tools (python3, /bin/sh) resolve without
// callers needing to re-specify PATH on every exec.
func buildEnv(override map[string]string) []string {
	return mergeEnv(os.Environ(), override)
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
