package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// connect wires a client to a server built with cfg over an in-memory
// transport and returns the live client session.
func connect(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(cfg)

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// daemonClient returns a client pointed at h, torn down with the test.
func daemonClient(t *testing.T, h http.Handler) *client.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return client.New(ts.URL)
}

// call invokes a tool and, on success, unmarshals its structured output into
// out. It fails the test on a tool error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s: unexpected tool error: %s", name, contentText(res))
	}
	if out != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("unmarshal %s output: %v", name, err)
		}
	}
}

// callErr invokes a tool expecting a tool error, returning its text.
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("call %s: expected a tool error, got success", name)
	}
	return contentText(res)
}

func contentText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestServerAdvertisesFullCatalog(t *testing.T) {
	cs := connect(t, Config{})

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	got := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
		if tl.InputSchema == nil {
			t.Errorf("tool %q advertises no input schema", tl.Name)
		}
		if tl.Description == "" {
			t.Errorf("tool %q advertises no description", tl.Name)
		}
	}
	sort.Strings(got)

	want := []string{
		"create_app", "create_sandbox", "delete_app", "delete_sandbox", "delete_snapshot",
		"exec", "fork", "get_app", "inspect_sandbox", "list_apps", "list_profiles",
		"list_sandboxes", "list_snapshots", "logs", "read_file", "run", "snapshot",
		"stop_sandbox", "update_app", "write_files",
	}
	if len(got) != len(want) {
		t.Fatalf("tools = %v (%d), want %d", got, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunToolCreatesExecsDeletes(t *testing.T) {
	var deletedID, createdProfile string
	var netAllow []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateSandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		createdProfile = req.Profile
		if req.Network != nil {
			netAllow = req.Network.Allowlist
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_run", Profile: req.Profile})
	})
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fw := wire.NewFrameWriter(w)
		_, _ = fw.Stream(wire.FrameStdout).Write([]byte("hello\n"))
		payload, _ := json.Marshal(wire.ExecResult{ExitCode: 7, DurationMs: 12})
		_ = fw.WriteFrame(wire.FrameExit, payload)
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletedID = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})

	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out execOutput
	call(t, cs, "run", map[string]any{
		"profile":   "python-3.12",
		"command":   []string{"echo", "hello"},
		"net_allow": []string{"pypi.org"},
	}, &out)

	if out.ExitCode != 7 {
		t.Errorf("exit_code = %d, want 7", out.ExitCode)
	}
	if out.Stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", out.Stdout, "hello\n")
	}
	if out.DurationMs != 12 {
		t.Errorf("duration_ms = %d, want 12", out.DurationMs)
	}
	if createdProfile != "python-3.12" {
		t.Errorf("daemon saw profile %q, want python-3.12", createdProfile)
	}
	if len(netAllow) != 1 || netAllow[0] != "pypi.org" {
		t.Errorf("daemon saw net allowlist %v, want [pypi.org]", netAllow)
	}
	if deletedID != "sbx_run" {
		t.Errorf("deleted id = %q, want sbx_run (run must always clean up)", deletedID)
	}
}

func TestCreateSandboxTool(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{
			ID: "sbx_1", Profile: "base", VCPUs: 2, MemoryMiB: 512,
			Network: &api.NetworkResponse{Enabled: true, GuestIP: "10.0.0.2", Allowlist: []string{"pypi.org"}},
		})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out sandboxOutput
	call(t, cs, "create_sandbox", map[string]any{"profile": "base", "vcpus": 2, "memory_mib": 512}, &out)

	if out.ID != "sbx_1" || out.VCPUs != 2 || out.MemoryMiB != 512 {
		t.Errorf("output = %+v", out)
	}
	if out.Network == nil || !out.Network.Enabled || out.Network.GuestIP != "10.0.0.2" {
		t.Errorf("network = %+v", out.Network)
	}
}

// TestCreateSandboxImagePublishDisk: the new create params reach the daemon
// (image+pull override profile; publish parses; disk_mib → disk_bytes), and
// the published mappings are echoed back in the tool output.
func TestCreateSandboxImagePublishDisk(t *testing.T) {
	var got api.CreateSandboxRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_img", Published: got.Publish})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out sandboxOutput
	call(t, cs, "create_sandbox", map[string]any{
		"image":    "nginx:alpine",
		"pull":     "always",
		"disk_mib": 2048,
		"publish":  []string{"8080:80"},
	}, &out)

	if got.Image == nil || got.Image.OCI != "nginx:alpine" || got.Pull != "always" {
		t.Errorf("daemon saw image=%+v pull=%q", got.Image, got.Pull)
	}
	if got.Profile != "" {
		t.Errorf("profile should be empty when image is set, got %q", got.Profile)
	}
	if got.DiskBytes != 2048<<20 {
		t.Errorf("disk_bytes = %d, want %d", got.DiskBytes, int64(2048)<<20)
	}
	if len(got.Publish) != 1 || got.Publish[0].HostPort != 8080 || got.Publish[0].GuestPort != 80 {
		t.Errorf("publish = %+v, want 8080->80", got.Publish)
	}
	if len(out.Published) != 1 || out.Published[0].HostPort != 8080 {
		t.Errorf("output published = %+v, want it echoed", out.Published)
	}
}

func TestCreateSandboxImageProfileMutuallyExclusive(t *testing.T) {
	cs := connect(t, Config{Client: client.New("http://127.0.0.1:0")})
	msg := callErr(t, cs, "create_sandbox", map[string]any{"image": "nginx", "profile": "base"})
	if !strings.Contains(msg, "mutually exclusive") {
		t.Errorf("error = %q, want mutually-exclusive", msg)
	}
}

func TestRunToolImage(t *testing.T) {
	var got api.CreateSandboxRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_run"})
	})
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, _ *http.Request) {
		fw := wire.NewFrameWriter(w)
		payload, _ := json.Marshal(wire.ExecResult{ExitCode: 0})
		_ = fw.WriteFrame(wire.FrameExit, payload)
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out execOutput
	call(t, cs, "run", map[string]any{
		"image":    "python:3.12-slim",
		"command":  []string{"python", "--version"},
		"disk_mib": 1024,
	}, &out)
	if got.Image == nil || got.Image.OCI != "python:3.12-slim" {
		t.Errorf("daemon saw image %+v, want python:3.12-slim", got.Image)
	}
	if got.DiskBytes != 1024<<20 {
		t.Errorf("disk_bytes = %d, want %d", got.DiskBytes, int64(1024)<<20)
	}
}

func TestLogsTool(t *testing.T) {
	var gotSource string
	var gotSince string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		gotSource = r.URL.Query().Get("source")
		gotSince = r.URL.Query().Get("since")
		_ = json.NewEncoder(w).Encode(api.LogsResponse{
			Records: []api.LogRecord{
				{TimeMs: 1, Source: "exec", Stream: "stdout", Text: "hello\n"},
			},
			NextOffset: 42,
		})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out logsOutput
	call(t, cs, "logs", map[string]any{"sandbox_id": "sbx_1", "source": "exec"}, &out)
	if gotSource != "exec" {
		t.Errorf("daemon saw source %q, want exec", gotSource)
	}
	if gotSince != "" {
		t.Errorf("daemon saw since %q, want it omitted (client tails on negative since)", gotSince)
	}
	if out.NextOffset != 42 || len(out.Records) != 1 || out.Records[0].Text != "hello\n" {
		t.Errorf("logs output = %+v", out)
	}

	if msg := callErr(t, cs, "logs", map[string]any{"sandbox_id": "sbx_1", "source": "bogus"}); !strings.Contains(msg, "source must be") {
		t.Errorf("bad source error = %q", msg)
	}
}

func TestStopSandboxTool(t *testing.T) {
	var stopped string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/service/stop", func(w http.ResponseWriter, r *http.Request) {
		stopped = r.PathValue("id")
		_ = json.NewEncoder(w).Encode(wire.ServiceStatus{})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out stoppedOutput
	call(t, cs, "stop_sandbox", map[string]any{"sandbox_id": "sbx_9"}, &out)
	if stopped != "sbx_9" {
		t.Errorf("daemon stopped %q, want sbx_9", stopped)
	}
	if out.Stopped != "sbx_9" {
		t.Errorf("output stopped = %q, want sbx_9", out.Stopped)
	}
}

func TestListProfilesTool(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /profiles", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProfilesResponse{Profiles: []string{"base", "python-3.12"}})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out profilesOutput
	call(t, cs, "list_profiles", map[string]any{}, &out)
	if len(out.Profiles) != 2 || out.Profiles[0] != "base" || out.Profiles[1] != "python-3.12" {
		t.Errorf("profiles = %v", out.Profiles)
	}
}

func TestForkTool(t *testing.T) {
	mux := http.NewServeMux()
	var gotCount int
	mux.HandleFunc("POST /snapshots/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		gotCount, _ = countFromQuery(r)
		_ = json.NewEncoder(w).Encode(api.ForkResponse{Sandboxes: []api.SandboxResponse{
			{ID: "sbx_a"}, {ID: "sbx_b"},
		}})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out forkOutput
	call(t, cs, "fork", map[string]any{"snapshot_id": "snap_1", "count": 2}, &out)
	if len(out.SandboxIDs) != 2 || out.SandboxIDs[0] != "sbx_a" || out.SandboxIDs[1] != "sbx_b" {
		t.Errorf("sandbox_ids = %v", out.SandboxIDs)
	}
	if gotCount != 2 {
		t.Errorf("daemon saw count %d, want 2", gotCount)
	}
}

func TestDeleteSnapshotTool(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /snapshots/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	var out deletedOutput
	call(t, cs, "delete_snapshot", map[string]any{"snapshot_id": "snap_9"}, &out)
	if out.Deleted != "snap_9" {
		t.Errorf("deleted = %q, want snap_9", out.Deleted)
	}
}

func TestToolErrorSurfacesDaemonMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "unknown profile \"ghost\""})
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	msg := callErr(t, cs, "create_sandbox", map[string]any{"profile": "ghost"})
	if !strings.Contains(msg, "unknown profile") {
		t.Errorf("error text = %q, want it to carry the daemon message", msg)
	}
}

func TestExecValidatesInput(t *testing.T) {
	// No daemon needed — validation short-circuits before any client call.
	cs := connect(t, Config{Client: client.New("http://127.0.0.1:0")})

	if msg := callErr(t, cs, "exec", map[string]any{"sandbox_id": "sbx_1"}); !strings.Contains(msg, "command") {
		t.Errorf("empty-command error = %q", msg)
	}
	if msg := callErr(t, cs, "exec", map[string]any{"command": []string{"ls"}}); !strings.Contains(msg, "sandbox_id") {
		t.Errorf("missing-sandbox error = %q", msg)
	}
	if msg := callErr(t, cs, "exec", map[string]any{"sandbox_id": "s", "command": []string{"ls"}, "env": []string{"BADENV"}}); !strings.Contains(msg, "KEY=VALUE") {
		t.Errorf("bad-env error = %q", msg)
	}
}

// countFromQuery reads the ?count= fork parameter.
func countFromQuery(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.URL.Query().Get("count"))
	return n, err == nil
}
