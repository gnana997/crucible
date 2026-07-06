package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
)

func TestCLISandboxLsTable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListResponse{Sandboxes: []api.SandboxResponse{
			{ID: "sbx_1", Profile: "python-3.12", VCPUs: 2, MemoryMiB: 512, CreatedAt: time.Now()},
		}})
	}))
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "sandbox", "ls"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "PROFILE") || !strings.Contains(s, "sbx_1") || !strings.Contains(s, "python-3.12") {
		t.Fatalf("table missing content:\n%s", s)
	}
}

func TestCLIProfileLsJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.ProfilesResponse{Profiles: []string{"base", "node-22"}})
	}))
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "-o", "json", "profile", "ls"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	var got []string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not json array: %v (%q)", err, out.String())
	}
	if len(got) != 2 || got[1] != "node-22" {
		t.Errorf("got %v", got)
	}
}

// stubDaemon builds a ServeMux that satisfies the run one-shot path
// (create → exec → delete), recording the deleted id and returning the
// given exec exit code.
func stubDaemon(t *testing.T, exitCode int, deleted *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_run"})
	})
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fw := agentwire.NewFrameWriter(w)
		_, _ = fw.Stream(agentwire.FrameStdout).Write([]byte("hi\n"))
		payload, _ := json.Marshal(agentwire.ExecResult{ExitCode: exitCode})
		_ = fw.WriteFrame(agentwire.FrameExit, payload)
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if deleted != nil {
			*deleted = r.PathValue("id")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestCLIRunCreatesExecsDeletes(t *testing.T) {
	var deleted string
	ts := stubDaemon(t, 0, &deleted)

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "run", "--", "echo", "hi"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "hi") {
		t.Errorf("stdout = %q; want streamed hi", out.String())
	}
	if deleted != "sbx_run" {
		t.Errorf("deleted = %q; want sbx_run (run should clean up)", deleted)
	}
}

func TestCLIRunPropagatesExitCode(t *testing.T) {
	ts := stubDaemon(t, 42, nil)
	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "run", "--", "false"}, &out, &errb)
	if code != 42 {
		t.Fatalf("exit=%d; want 42 (propagated from exec)", code)
	}
}

func TestCLIRunKeepSkipsDelete(t *testing.T) {
	var deleted string
	ts := stubDaemon(t, 0, &deleted)
	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "run", "--keep", "--", "echo", "hi"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if deleted != "" {
		t.Errorf("deleted = %q; --keep should skip deletion", deleted)
	}
}
