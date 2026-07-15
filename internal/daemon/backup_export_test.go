package daemon

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// exportTestServer wires a real volume Manager (skips without mkfs.ext4) plus a
// token store holding a volume_backup-scoped key and a read-only key, and
// returns the server, both raw keys, and one backup id to export.
func exportTestServer(t *testing.T) (srv *Server, backupID, backupPath, bkTok, roTok string) {
	t.Helper()
	dir := t.TempDir()
	vm, err := volume.NewManager(dir, 8<<20, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Skipf("volume manager unavailable: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	if _, err := vm.Create("data", 8<<20, volume.CreateOpts{}); err != nil {
		t.Fatalf("Create volume: %v", err)
	}
	b, err := vm.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	tokPath := filepath.Join(dir, "tokens.json")
	bk, _, err := tokenstore.Add(tokPath, tokenstore.AddOptions{
		Name:   "cp",
		Policy: &policy.Policy{Operations: []policy.Operation{policy.OpVolumeBackup}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ro, _, err := tokenstore.Add(tokPath, tokenstore.AddOptions{
		Name:   "ro",
		Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead}},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, err = New(Config{
		Manager:    stubSandboxManager(t),
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Volumes:    vm,
		TokenStore: tokenstore.Open(tokPath),
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv, b.ID, b.Path, bk, ro
}

func getRaw(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func TestExportBackupGzipRoundTrips(t *testing.T) {
	srv, id, path, bkTok, _ := exportTestServer(t)
	onDisk, _ := os.ReadFile(path)

	rec := getRaw(t, srv, "/backups/"+id+"/export", bkTok)
	if rec.Code != 200 {
		t.Fatalf("export status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if got := rec.Header().Get("X-Crucible-Backup-Size"); got != strconv.Itoa(len(onDisk)) {
		t.Errorf("X-Crucible-Backup-Size = %q, want %d", got, len(onDisk))
	}
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(got, onDisk) {
		t.Error("decompressed export differs from the backup file")
	}
}

func TestExportBackupRaw(t *testing.T) {
	srv, id, path, bkTok, _ := exportTestServer(t)
	onDisk, _ := os.ReadFile(path)

	rec := getRaw(t, srv, "/backups/"+id+"/export?compress=none", bkTok)
	if rec.Code != 200 {
		t.Fatalf("raw export status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(onDisk)) {
		t.Errorf("Content-Length = %q, want %d (raw stream must set it)", cl, len(onDisk))
	}
	if !bytes.Equal(rec.Body.Bytes(), onDisk) {
		t.Error("raw export differs from the backup file")
	}
}

func TestExportBackupNeedsVolumeBackupOp(t *testing.T) {
	srv, id, _, _, roTok := exportTestServer(t)
	// A read-only token must NOT stream volume data off the host.
	if rec := getRaw(t, srv, "/backups/"+id+"/export", roTok); rec.Code != 403 {
		t.Errorf("read-only token: export = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestExportBackupMissing404(t *testing.T) {
	srv, _, _, bkTok, _ := exportTestServer(t)
	if rec := getRaw(t, srv, "/backups/nope-nonexistent/export", bkTok); rec.Code != 404 {
		t.Errorf("missing backup export = %d, want 404", rec.Code)
	}
}

func postRaw(t *testing.T, srv *Server, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// TestImportBackupRoundTripOverHTTP exports a backup (gzip) and imports it back
// through the HTTP handlers, then confirms the imported backup file matches —
// the export→import loop across the API, with the volume_backup gate.
func TestImportBackupRoundTripOverHTTP(t *testing.T) {
	srv, id, path, bkTok, roTok := exportTestServer(t)

	// Import needs its own op, not read.
	if rec := postRaw(t, srv, "/backups/import?source=data", roTok, []byte("x")); rec.Code != 403 {
		t.Fatalf("read-only import = %d, want 403", rec.Code)
	}
	// source is required.
	if rec := postRaw(t, srv, "/backups/import", bkTok, []byte("x")); rec.Code != 400 {
		t.Fatalf("import without source = %d, want 400", rec.Code)
	}

	// Export (gzip) → import (gzip) over HTTP.
	exp := getRaw(t, srv, "/backups/"+id+"/export", bkTok)
	if exp.Code != 200 {
		t.Fatalf("export = %d", exp.Code)
	}
	imp := postRaw(t, srv, "/backups/import?source=data", bkTok, exp.Body.Bytes())
	if imp.Code != 201 {
		t.Fatalf("import = %d, want 201 (%s)", imp.Code, imp.Body.String())
	}
	var rec api.Backup
	if err := json.Unmarshal(imp.Body.Bytes(), &rec); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if rec.ID == id {
		t.Error("import should mint a fresh id")
	}
	// The imported backing file is byte-identical to the original backup file.
	orig, _ := os.ReadFile(path)
	f, _, _, err := srv.cfg.Volumes.OpenBackup(rec.ID)
	if err != nil {
		t.Fatalf("OpenBackup(imported): %v", err)
	}
	defer func() { _ = f.Close() }()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, orig) {
		t.Error("imported backup differs from the original after the HTTP round trip")
	}
}
