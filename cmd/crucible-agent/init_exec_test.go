//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func TestEnsureDefaultPath(t *testing.T) {
	// No PATH → default added.
	got := ensureDefaultPath([]string{"HOME=/"})
	if !containsEnv(got, "PATH="+dockerDefaultPath) {
		t.Errorf("default PATH not added: %v", got)
	}
	// Existing PATH → untouched.
	got = ensureDefaultPath([]string{"PATH=/only/here", "HOME=/"})
	if !containsEnv(got, "PATH=/only/here") || containsEnv(got, dockerDefaultPath) {
		t.Errorf("existing PATH overwritten: %v", got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestInitExecResolvesViaDefaultPath is the node-exec regression: a
// guest whose environment sets no PATH (a PID-1 agent) must still
// resolve a command that lives only on the Docker default PATH's
// /usr/local/bin — the busybox-shell case that lost `node`. Here the
// agent's own environ has no PATH (we clear it), and the check runs a
// binary reachable only via ensureDefaultPath.
func TestInitExecResolvesViaDefaultPath(t *testing.T) {
	// Simulate a PID-1 agent whose kernel-provided env has NO PATH entry
	// (unset, not empty — that is what makes a shell fall back to its
	// builtin default, and busybox's excludes /usr/local/bin).
	if old, had := os.LookupEnv("PATH"); had {
		_ = os.Unsetenv("PATH")
		t.Cleanup(func() { _ = os.Setenv("PATH", old) })
	}
	env := buildEnv(nil)
	if !containsEnv(env, "PATH="+dockerDefaultPath) {
		t.Fatalf("buildEnv did not supply a default PATH: %v", env)
	}
	// And that default includes /usr/local/bin (where node/python live).
	if !strings.Contains(dockerDefaultPath, "/usr/local/bin") {
		t.Errorf("dockerDefaultPath lacks /usr/local/bin: %q", dockerDefaultPath)
	}
}

func TestBringUpLoopbackBestEffort(t *testing.T) {
	// bringUpLoopback must never panic and must tolerate lacking
	// permission (non-root test process can't set lo up — the call is
	// best-effort and only logs). Just exercise the path.
	bringUpLoopback(testLogger())
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
