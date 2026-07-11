package crucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL)
}

func TestCreateSandbox(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var req api.CreateSandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Profile != "python-3.12" {
			t.Errorf("profile = %q, want python-3.12", req.Profile)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "sbx_1", Profile: req.Profile, VCPUs: 2})
	})
	sb, err := c.CreateSandbox(context.Background(), api.CreateSandboxRequest{Profile: "python-3.12"})
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != "sbx_1" || sb.Profile != "python-3.12" {
		t.Errorf("got %+v", sb)
	}
}

func TestListSandboxesPage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListResponse{Sandboxes: []api.SandboxResponse{{ID: "a"}, {ID: "b"}}})
	})
	page, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "a" || page.Items[1].ID != "b" {
		t.Errorf("got %+v", page.Items)
	}
	if page.NextCursor != "" {
		t.Errorf("single-node daemon must not return a cursor, got %q", page.NextCursor)
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "unknown sandbox"})
	})
	_, err := c.GetSandbox(context.Background(), "sbx_x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteSandboxNoContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteSandbox(context.Background(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
}

func TestForkPassesCount(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("count"); got != "3" {
			t.Errorf("count = %q, want 3", got)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.ForkResponse{Sandboxes: []api.SandboxResponse{{ID: "f1"}, {ID: "f2"}, {ID: "f3"}}})
	})
	forks, err := c.Fork(context.Background(), "snap_1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 3 {
		t.Errorf("got %d forks, want 3", len(forks))
	}
}

func TestListProfiles(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProfilesResponse{Profiles: []string{"base", "node-22"}})
	})
	profs, err := c.ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profs) != 2 || profs[1] != "node-22" {
		t.Errorf("got %v", profs)
	}
}

func TestExecStreamsAndReturnsResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fw := wire.NewFrameWriter(w)
		_, _ = fw.Stream(wire.FrameStdout).Write([]byte("hello"))
		_, _ = fw.Stream(wire.FrameStderr).Write([]byte("warn"))
		payload, _ := json.Marshal(wire.ExecResult{ExitCode: 7, DurationMs: 5})
		_ = fw.WriteFrame(wire.FrameExit, payload)
	})
	var out, errb bytes.Buffer
	res, err := c.Exec(context.Background(), "sbx_1", wire.ExecRequest{Cmd: []string{"x"}}, &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" {
		t.Errorf("stdout = %q, want hello", out.String())
	}
	if errb.String() != "warn" {
		t.Errorf("stderr = %q, want warn", errb.String())
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res.ExitCode)
	}
}

func TestExecPreStreamErrorNotStreamed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "cmd is required"})
	})
	_, err := c.Exec(context.Background(), "sbx_1", wire.ExecRequest{}, nil, nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("cmd is required")) {
		t.Fatalf("err = %v, want cmd-required error", err)
	}
}

func TestTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		sentinel error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrPolicyDenied},
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "nope"})
		})
		_, err := c.GetSandbox(context.Background(), "sbx_x")
		if !errors.Is(err, tc.sentinel) {
			t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.sentinel)
		}
		var de *Error
		if !errors.As(err, &de) || de.Status != tc.status || de.Message != "nope" {
			t.Fatalf("status %d: not a structured *Error: %v", tc.status, err)
		}
	}
}
