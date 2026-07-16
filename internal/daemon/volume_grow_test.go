package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// growTestServer wires a real volume Manager with one 8 MiB volume "data".
func growTestServer(t *testing.T) (*Server, *volume.Manager) {
	t.Helper()
	if _, err := exec.LookPath("resize2fs"); err != nil {
		t.Skip("resize2fs not available")
	}
	dir := t.TempDir()
	vm, err := volume.NewManager(dir, 8<<20, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Skipf("volume manager unavailable: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	if _, err := vm.Create("data", 8<<20, volume.CreateOpts{}); err != nil {
		t.Fatalf("Create volume: %v", err)
	}
	srv, err := New(Config{
		Manager:    stubSandboxManager(t),
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Volumes:    vm,
		TokenStore: tokenstore.Open(""),
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv, vm
}

func postGrow(t *testing.T, srv *Server, name string, sizeBytes int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(api.GrowVolumeRequest{SizeBytes: sizeBytes})
	r := httptest.NewRequest(http.MethodPost, "/volumes/"+name+"/grow", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func TestGrowVolumeHandler(t *testing.T) {
	srv, _ := growTestServer(t)
	rec := postGrow(t, srv, "data", 24<<20)
	if rec.Code != http.StatusOK {
		t.Fatalf("grow status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var v api.Volume
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.SizeBytes != 24<<20 {
		t.Fatalf("returned size = %d, want %d", v.SizeBytes, 24<<20)
	}
}

func TestGrowVolumeHandlerRefusesShrink(t *testing.T) {
	srv, _ := growTestServer(t)
	rec := postGrow(t, srv, "data", 4<<20)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("shrink status = %d, want 400", rec.Code)
	}
}

func TestGrowVolumeHandlerRefusesAttached(t *testing.T) {
	srv, vm := growTestServer(t)
	if _, _, err := vm.Attach("data", "sbx1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer vm.Release("data")
	rec := postGrow(t, srv, "data", 24<<20)
	if rec.Code != http.StatusConflict {
		t.Fatalf("attached grow status = %d, want 409", rec.Code)
	}
}
