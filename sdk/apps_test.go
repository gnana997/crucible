package crucible

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func TestAppCRUD(t *testing.T) {
	var hits []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apps":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "app_1", AppSpec: api.AppSpec{Name: "web"}, DesiredState: "running"})
		case r.URL.Path == "/apps":
			_ = json.NewEncoder(w).Encode(api.AppListResponse{Apps: []api.AppResponse{{ID: "app_1"}, {ID: "app_2"}}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default: // GET /apps/web
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "app_1", AppSpec: api.AppSpec{Name: "web"},
				Status: &api.AppStatus{InstanceID: "sbx_9", Phase: "running", Health: "healthy"}})
		}
	})
	ctx := context.Background()

	created, err := c.CreateApp(ctx, api.CreateAppRequest{AppSpec: api.AppSpec{Name: "web", Image: &api.ImageRef{OCI: "nginx"}}})
	if err != nil || created.ID != "app_1" {
		t.Fatalf("CreateApp: %+v err=%v", created, err)
	}
	page, err := c.ListApps(ctx)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("ListApps: %d err=%v", len(page.Items), err)
	}
	got, err := c.GetApp(ctx, "web")
	if err != nil || got.Status.InstanceID != "sbx_9" {
		t.Fatalf("GetApp: %+v err=%v", got, err)
	}
	if err := c.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
}

// App.Exec hits the app-scoped route directly; the daemon resolves the current
// instance server-side (redeploy-safe), so the client no longer GETs the app
// first.
func TestAppHandleExecTargetsAppRoute(t *testing.T) {
	var got string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		if r.Method == http.MethodPost && r.URL.Path == "/apps/web/exec" {
			w.WriteHeader(http.StatusOK)
			fw := wire.NewFrameWriter(w)
			payload, _ := json.Marshal(wire.ExecResult{ExitCode: 0})
			_ = fw.WriteFrame(wire.FrameExit, payload)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	res, err := c.App("web").Exec(context.Background(), wire.ExecRequest{Cmd: []string{"true"}}, nil, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("App.Exec: %+v err=%v", res, err)
	}
	if got != "POST /apps/web/exec" {
		t.Errorf("App.Exec hit %q, want POST /apps/web/exec", got)
	}
}

// The daemon answers 409 when the app has no running instance; App.Exec
// surfaces it as an error.
func TestAppHandleExecNoInstance(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "app web has no running instance"})
	})
	_, err := c.App("web").Exec(context.Background(), wire.ExecRequest{Cmd: []string{"true"}}, nil, nil)
	if err == nil {
		t.Fatal("expected error when app has no instance")
	}
}
