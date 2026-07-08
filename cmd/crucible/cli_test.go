package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

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

// unresolvableImage is an image ref no local Docker will have, so
// resolveCreateImage passes it through unchanged (no docker-save attempt) —
// keeps these CLI tests deterministic regardless of the host's Docker state.
const unresolvableImage = "crucible.invalid/no-such-image:latest"

// TestCLIRunImageCreatesLongLived: `run <image> -p ...` boots the image,
// echoes the sandbox id, applies the publish mapping, and does NOT delete
// (image runs are long-lived by default).
func TestCLIRunImageCreatesLongLived(t *testing.T) {
	var created api.CreateSandboxRequest
	var deleted string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_img", Published: created.Publish})
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "run", unresolvableImage, "-p", "8080:80"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if created.Image == nil || created.Image.OCI != unresolvableImage {
		t.Errorf("create Image = %+v; want OCI=%q", created.Image, unresolvableImage)
	}
	if len(created.Publish) != 1 || created.Publish[0].HostPort != 8080 || created.Publish[0].GuestPort != 80 {
		t.Errorf("create Publish = %+v; want 8080->80", created.Publish)
	}
	if !strings.Contains(out.String(), "sbx_img") {
		t.Errorf("stdout = %q; want the sandbox id", out.String())
	}
	if deleted != "" {
		t.Errorf("deleted = %q; a plain image run is long-lived (no delete)", deleted)
	}
}

// TestRunImageRmDeletesOnDetach drives the --rm path directly (the run()
// entry uses a non-cancellable context): it tails logs, and when the context
// is cancelled it removes the sandbox.
func TestRunImageRmDeletesOnDetach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var deleted string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_rm"})
	})
	mux.HandleFunc("GET /sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		cancel() // detach as soon as we start tailing
		_ = json.NewEncoder(w).Encode(api.LogsResponse{})
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	o := &globalOpts{addr: ts.URL, output: "table"}
	c := &cobra.Command{}
	c.SetContext(ctx)
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)

	if err := runImage(c, o, unresolvableImage, runImageOpts{rm: true}); err != nil {
		t.Fatalf("runImage --rm: %v", err)
	}
	if deleted != "sbx_rm" {
		t.Errorf("deleted = %q; --rm should remove the sandbox on detach", deleted)
	}
}

func TestCLIStopGracefullyStopsService(t *testing.T) {
	var stopped string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/service/stop", func(w http.ResponseWriter, r *http.Request) {
		stopped = r.PathValue("id")
		_ = json.NewEncoder(w).Encode(agentwire.ServiceStatus{})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "stop", "sbx_1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if stopped != "sbx_1" {
		t.Errorf("stopped = %q; want sbx_1", stopped)
	}
	if !strings.Contains(out.String(), "sbx_1") {
		t.Errorf("stdout = %q; want the stopped id", out.String())
	}
}

func TestParseDiskSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"1024", 1024},
		{"512M", 512 << 20},
		{"2G", 2 << 30},
		{"2g", 2 << 30},
		{"2GB", 2 << 30},
		{"2GiB", 2 << 30},
		{"4K", 4 << 10},
		{"1T", 1 << 40},
		{" 8G ", 8 << 30},
	}
	for _, c := range ok {
		got, err := parseDiskSize(c.in)
		if err != nil {
			t.Errorf("parseDiskSize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDiskSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"abc", "G", "-2G", "2X", "1.5G"} {
		if _, err := parseDiskSize(bad); err == nil {
			t.Errorf("parseDiskSize(%q) = nil error, want failure", bad)
		}
	}
}

// TestCLICreateDiskThreadsBytes: `create --disk 2G` sends disk_bytes on the wire.
func TestCLICreateDiskThreadsBytes(t *testing.T) {
	var created api.CreateSandboxRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_disk"})
	}))
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "sandbox", "create", "--disk", "2G"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if created.DiskBytes != 2<<30 {
		t.Errorf("DiskBytes = %d, want %d", created.DiskBytes, int64(2<<30))
	}
}

func TestCLIRmDeletesSandbox(t *testing.T) {
	var deleted string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--addr", ts.URL, "rm", "sbx_1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if deleted != "sbx_1" {
		t.Errorf("deleted = %q; want sbx_1", deleted)
	}
}
