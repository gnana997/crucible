//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// collectFrames drains a /exec response body and returns the stdout
// bytes, stderr bytes, and the terminal ExecResult.
func collectFrames(t *testing.T, body io.Reader) (stdout, stderr []byte, result agentwire.ExecResult) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	sawExit := false
	for {
		f, err := agentwire.ReadFrame(body)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		switch f.Type {
		case agentwire.FrameStdout:
			stdoutBuf.Write(f.Payload)
		case agentwire.FrameStderr:
			stderrBuf.Write(f.Payload)
		case agentwire.FrameExit:
			if err := json.Unmarshal(f.Payload, &result); err != nil {
				t.Fatalf("decode exit frame: %v", err)
			}
			sawExit = true
		default:
			t.Errorf("unexpected frame type %d", f.Type)
		}
	}
	if !sawExit {
		t.Error("no exit frame in response")
	}
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), result
}

func TestHandleExecHello(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"cmd": ["/bin/echo", "hello from guest"]}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	stdout, stderr, result := collectFrames(t, resp.Body)
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d (err=%q), want 0", result.ExitCode, result.Error)
	}
	if got := strings.TrimRight(string(stdout), "\n"); got != "hello from guest" {
		t.Errorf("stdout = %q, want %q", got, "hello from guest")
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if result.DurationMs < 0 {
		t.Errorf("DurationMs = %d, want >= 0", result.DurationMs)
	}
}

func TestHandleExecStderrAndNonZeroExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"cmd": ["/bin/sh", "-c", "echo out; echo err 1>&2; exit 7"]}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	stdout, stderr, result := collectFrames(t, resp.Body)
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
	if got := strings.TrimRight(string(stdout), "\n"); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := strings.TrimRight(string(stderr), "\n"); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
}

func TestHandleExecCommandNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"cmd": ["/nonexistent/definitely-not-here"]}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	_, _, result := collectFrames(t, resp.Body)
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.Error == "" {
		t.Error("Error = empty, want populated")
	}
}

func TestHandleExecTimedOut(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"cmd": ["/bin/sleep", "5"], "timeout_s": 1}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	_, _, result := collectFrames(t, resp.Body)
	if !result.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.Signal != "SIGKILL" {
		t.Errorf("Signal = %q, want SIGKILL", result.Signal)
	}
}

func TestHandleExecRejectsEmptyCmd(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"cmd": []}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleExecRejectsBadJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
