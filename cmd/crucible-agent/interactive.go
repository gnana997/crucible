//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gnana997/crucible/sdk/wire"
)

// handleExecInteractive is the profile-mode (systemd is PID 1) interactive
// exec handler. Unlike handleExec, it hijacks the raw connection and runs
// the process with a live stdin fed by inbound FrameStdin frames — a
// functional (no-PTY) shell with persistent cwd/env. Response framing is
// identical to the one-shot path (FrameStdout/FrameStderr, terminal
// FrameExit), so host-side readers are unchanged.
func handleExecInteractive(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req, ok := decodeExecRequest(w, r)
	if !ok {
		return
	}

	conn, br, err := hijackExec(w)
	if err != nil {
		slog.Error("interactive exec: hijack failed", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	fw := wire.NewFrameWriter(conn)

	// The request context is no longer managed once the conn is hijacked, so
	// derive a fresh command context (with the optional deadline) here. A
	// client disconnect is surfaced by pumpStdin instead.
	cmdCtx, cancel := commandContext(context.Background(), req.TimeoutSec)
	defer cancel()

	// io.Pipe bridges inbound stdin frames to the child's stdin. os/exec
	// spawns an internal copier from stdinR to the child; WaitDelay (set by
	// configureExecProcess) unblocks it if the client never closes stdin.
	stdinR, stdinW := io.Pipe()
	cmd := exec.CommandContext(cmdCtx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Env = buildEnv(req.Env)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Stdin = stdinR
	cmd.Stdout = fw.Stream(wire.FrameStdout)
	cmd.Stderr = fw.Stream(wire.FrameStderr)
	configureExecProcess(cmd)

	if err := cmd.Start(); err != nil {
		writeExitFrame(fw, resultFromError(err, cmd, cmdCtx.Err(), time.Since(start)))
		return
	}

	// A read error from the client (as opposed to a graceful FrameStdinClose)
	// means the client vanished — cancel the command so its group is killed.
	go pumpStdin(br, stdinW, cancel)

	runErr := cmd.Wait()
	_ = stdinR.Close()
	result := resultFromError(runErr, cmd, cmdCtx.Err(), time.Since(start))
	writeExitFrame(fw, result)

	slog.Info("interactive exec completed",
		"cmd", req.Cmd,
		"exit_code", result.ExitCode,
		"duration_ms", result.DurationMs,
	)
}

// handleExecInitInteractive is the PID-1 (OCI image) interactive exec
// handler. It mirrors handleExecInteractive but spawns through the reaper —
// in init mode no code may call os/exec Wait (it would race the reaper's
// wait4). See handleExecInit for the one-shot counterpart.
func (r *reaper) handleExecInitInteractive(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	er, ok := decodeExecRequest(w, req)
	if !ok {
		return
	}

	conn, br, err := hijackExec(w)
	if err != nil {
		slog.Error("interactive exec: hijack failed (init)", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	fw := wire.NewFrameWriter(conn)

	ctx, cancel := commandContext(context.Background(), er.TimeoutSec)
	defer cancel()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		writeExitFrame(fw, wire.ExecResult{
			ExitCode: -1, Error: err.Error(), DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	// spawn closes stdinR (the child's end) on success; on failure we own
	// both ends and must close them ourselves.
	rp, err := r.spawn(er.Cmd, buildEnv(er.Env), er.Cwd, nil, stdinR,
		fw.Stream(wire.FrameStdout), fw.Stream(wire.FrameStderr))
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		writeExitFrame(fw, wire.ExecResult{
			ExitCode: -1, Error: err.Error(), DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	// Kill the whole group on deadline or client disconnect; stop the
	// watcher once the process is reaped.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rp.signal(syscall.SIGKILL)
		case <-done:
		}
	}()

	go pumpStdin(br, stdinW, cancel)

	res := rp.wait()
	close(done)

	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	result := execResultFromWait(res, nil, time.Since(start), timedOut, ctx.Err() != nil)
	writeExitFrame(fw, result)

	slog.Info("interactive exec completed (init)",
		"cmd", er.Cmd,
		"exit_code", result.ExitCode,
		"duration_ms", result.DurationMs,
	)
}

// decodeExecRequest parses and validates the JSON ExecRequest body shared by
// every exec path. It writes a plain 4xx and returns ok=false on failure —
// safe to call before any hijack, while normal HTTP error responses still
// work.
func decodeExecRequest(w http.ResponseWriter, r *http.Request) (wire.ExecRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxExecRequestBody)
	var req wire.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return req, false
	}
	if len(req.Cmd) == 0 {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// hijackExec takes over the raw connection behind w, writes the streamed
// 200 response line ourselves (net/http won't after a hijack), and returns
// the conn plus the buffered reader that may already hold inbound frame
// bytes read alongside the request. Read inbound frames from the returned
// reader, not the conn, or those buffered bytes are lost.
func hijackExec(w http.ResponseWriter) (net.Conn, *bufio.Reader, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("interactive exec: response writer is not a hijacker")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, buf.Reader, nil
}

// pumpStdin reads inbound frames from the client and feeds FrameStdin
// payloads to the process's stdin. A FrameStdinClose closes stdin (EOF to
// the process) and stops reading — the process may keep running and
// producing output. Any read error is treated as the client disconnecting:
// stdin is closed and onDisconnect fires so the caller can kill the process.
func pumpStdin(r *bufio.Reader, stdin io.WriteCloser, onDisconnect func()) {
	for {
		f, err := wire.ReadFrame(r)
		if err != nil {
			_ = stdin.Close()
			onDisconnect()
			return
		}
		switch f.Type {
		case wire.FrameStdin:
			if len(f.Payload) > 0 {
				if _, err := stdin.Write(f.Payload); err != nil {
					// Process stdin is gone (exited/closed). Nothing more we
					// can deliver; treat as a disconnect and stop.
					onDisconnect()
					return
				}
			}
		case wire.FrameStdinClose:
			_ = stdin.Close()
			return
		default:
			// Ignore unknown inbound frame types (forward-compatible).
		}
	}
}
