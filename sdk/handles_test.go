package crucible

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// TestHandlesDelegate drives the sugar chain create→exec→snapshot→fork→
// delete against a fake daemon and asserts every hop hits the right route
// with the right ID — the whole point of handles is that they can't drift
// from the flat methods.
func TestHandlesDelegate(t *testing.T) {
	var hits []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/sandboxes/sbx_1/exec":
			w.WriteHeader(http.StatusOK)
			fw := wire.NewFrameWriter(w)
			_, _ = fw.Stream(wire.FrameStdout).Write([]byte("ok"))
			payload, _ := json.Marshal(wire.ExecResult{ExitCode: 0})
			_ = fw.WriteFrame(wire.FrameExit, payload)
		case r.URL.Path == "/sandboxes/sbx_1/snapshot":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.SnapshotResponse{ID: "snap_1"})
		case r.URL.Path == "/snapshots/snap_1/fork":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.ForkResponse{Sandboxes: []api.SandboxResponse{{ID: "f1"}, {ID: "f2"}}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(wire.ServiceStatus{State: wire.ServiceStateIdle})
		}
	})
	ctx := context.Background()

	sbx := c.Sandbox("sbx_1")
	var out bytes.Buffer
	res, err := sbx.Exec(ctx, wire.ExecRequest{Cmd: []string{"true"}}, &out, nil)
	if err != nil || res.ExitCode != 0 || out.String() != "ok" {
		t.Fatalf("exec via handle: res=%+v err=%v out=%q", res, err, out.String())
	}
	if _, err := sbx.ServiceStatus(ctx); err != nil {
		t.Fatalf("service status via handle: %v", err)
	}

	snap, err := sbx.Snapshot(ctx)
	if err != nil || snap.ID != "snap_1" {
		t.Fatalf("snapshot via handle: %+v err=%v", snap, err)
	}
	forks, err := snap.Fork(ctx, 2)
	if err != nil || len(forks) != 2 || forks[0].ID != "f1" {
		t.Fatalf("fork via handle: %+v err=%v", forks, err)
	}
	if err := forks[1].Delete(ctx); err != nil {
		t.Fatalf("delete via handle: %v", err)
	}

	want := []string{
		"POST /sandboxes/sbx_1/exec",
		"GET /sandboxes/sbx_1/service",
		"POST /sandboxes/sbx_1/snapshot",
		"POST /snapshots/snap_1/fork",
		"DELETE /sandboxes/f2",
	}
	if len(hits) != len(want) {
		t.Fatalf("routes hit = %v, want %v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("hop %d = %q, want %q", i, hits[i], want[i])
		}
	}
}
