//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// readExecFrames drives an init-mode /exec response and returns the
// concatenated stdout, stderr, and the terminal ExecResult.
func readExecFrames(t *testing.T, body io.Reader) (string, string, agentwire.ExecResult) {
	t.Helper()
	var out, errOut bytes.Buffer
	var result agentwire.ExecResult
	gotExit := false
	for {
		frame, err := agentwire.ReadFrame(body)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch frame.Type {
		case agentwire.FrameStdout:
			out.Write(frame.Payload)
		case agentwire.FrameStderr:
			errOut.Write(frame.Payload)
		case agentwire.FrameExit:
			if err := json.Unmarshal(frame.Payload, &result); err != nil {
				t.Fatalf("decode exit frame: %v", err)
			}
			gotExit = true
		}
	}
	if !gotExit {
		t.Fatal("no exit frame")
	}
	return out.String(), errOut.String(), result
}

func initExecServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := newTestReaper(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", r.handleExecInit)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postExec(t *testing.T, ts *httptest.Server, req agentwire.ExecRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	return resp
}

func TestInitExecHelloAndExit(t *testing.T) {
	ts := initExecServer(t)
	resp := postExec(t, ts, agentwire.ExecRequest{Cmd: []string{"/bin/sh", "-c", "echo hi; echo oops 1>&2; exit 5"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	out, errOut, res := readExecFrames(t, resp.Body)
	if out != "hi\n" || errOut != "oops\n" {
		t.Errorf("out=%q err=%q", out, errOut)
	}
	if res.ExitCode != 5 {
		t.Errorf("exit code = %d, want 5", res.ExitCode)
	}
	if res.Usage == nil {
		t.Error("usage not populated")
	}
}

func TestInitExecTimeout(t *testing.T) {
	ts := initExecServer(t)
	resp := postExec(t, ts, agentwire.ExecRequest{Cmd: []string{"/bin/sleep", "10"}, TimeoutSec: 1})
	defer func() { _ = resp.Body.Close() }()
	_, _, res := readExecFrames(t, resp.Body)
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("res = %+v, want timed_out with exit -1", res)
	}
	if res.DurationMs > 5000 {
		t.Errorf("timeout took %dms, expected ~1s", res.DurationMs)
	}
}

func TestInitExecCommandNotFound(t *testing.T) {
	ts := initExecServer(t)
	resp := postExec(t, ts, agentwire.ExecRequest{Cmd: []string{"/nope/missing"}})
	defer func() { _ = resp.Body.Close() }()
	_, _, res := readExecFrames(t, resp.Body)
	if res.ExitCode != -1 || res.Error == "" {
		t.Errorf("res = %+v, want -1 with error", res)
	}
}

func TestInitExecRejectsEmptyCmd(t *testing.T) {
	ts := initExecServer(t)
	resp := postExec(t, ts, agentwire.ExecRequest{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty cmd = %d, want 400", resp.StatusCode)
	}
}

func TestInitExecEnvAndCwd(t *testing.T) {
	ts := initExecServer(t)
	dir := t.TempDir()
	resp := postExec(t, ts, agentwire.ExecRequest{
		Cmd: []string{"/bin/sh", "-c", `printf '%s %s' "$CRUCIBLE_E" "$PWD"`},
		Env: map[string]string{"CRUCIBLE_E": "val"},
		Cwd: dir,
	})
	defer func() { _ = resp.Body.Close() }()
	out, _, res := readExecFrames(t, resp.Body)
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d", res.ExitCode)
	}
	if out != "val "+dir {
		t.Errorf("out = %q, want %q", out, "val "+dir)
	}
}

func TestMountSpecOrdering(t *testing.T) {
	// Parents must precede children so the mount sequence is valid.
	seen := map[string]bool{}
	for _, m := range pseudoMounts {
		if m.target == "/dev/pts" && !seen["/dev"] {
			t.Error("/dev/pts mounted before /dev")
		}
		if m.target == "/sys/fs/cgroup" && !seen["/sys"] {
			t.Error("cgroup mounted before /sys")
		}
		seen[m.target] = true
	}
	if !seen["/proc"] || !seen["/dev"] {
		t.Error("core mounts /proc and /dev must be present")
	}
}
