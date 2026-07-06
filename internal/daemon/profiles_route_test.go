package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/sandbox"
)

func TestProfilesRouteReturnsSortedNames(t *testing.T) {
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{t: t},
		WorkBase: t.TempDir(),
		Kernel:   "/fake/vmlinux",
		Rootfs:   tmpl,
		Profiles: map[string]string{"node-22": "/img/node-22.ext4", "base": "/img/base.ext4"},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv, err := New(Config{
		Manager: mgr,
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/profiles", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /profiles = %d, want 200", rec.Code)
	}
	var got api.ProfilesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Profiles) != 2 || got.Profiles[0] != "base" || got.Profiles[1] != "node-22" {
		t.Errorf("profiles = %v, want [base node-22] sorted", got.Profiles)
	}
}
