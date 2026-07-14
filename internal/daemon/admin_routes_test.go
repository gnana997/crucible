package daemon

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/registryauth"
	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/sdk/api"
	bolt "go.etcd.io/bbolt"
)

// newBackupTestServer builds a daemon over real stores: an app store holding
// one record, a token file, and a registry-credential file. Volumes stay nil
// (needs mkfs.ext4) — the nil-component-omitted path is covered instead.
func newBackupTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()

	store, err := app.Open(filepath.Join(dir, "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Put(app.Record{ID: "app_backup1", Spec: api.AppSpec{Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	amgr := app.NewManager(store, &fakeInst{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tokPath := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(tokPath, []byte(`{"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	regStore := registryauth.Open(filepath.Join(dir, "registry.json"))
	if err := regStore.Upsert("ghcr.io", "user", "secret"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		Manager:       stubSandboxManager(t),
		Addr:          "127.0.0.1:0",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppManager:    amgr,
		TokenStore:    tokenstore.Open(tokPath),
		RegistryStore: regStore,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestAdminBackupStreamsAllStores(t *testing.T) {
	ts := newBackupTestServer(t)

	resp, err := http.Get(ts.URL + "/admin/backup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		if int64(len(b)) != hdr.Size {
			t.Errorf("%s: read %d bytes, header said %d", hdr.Name, len(b), hdr.Size)
		}
		entries[hdr.Name] = b
	}

	for _, want := range []string{"app.db", "tokens.json", "registry-credentials.json", "manifest.json"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive missing %s (has %v)", want, keys(entries))
		}
	}
	if _, ok := entries["volume-index.db"]; ok {
		t.Error("volume-index.db present despite nil volume manager")
	}

	// The app.db copy must be a valid bbolt file holding the record.
	dbPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(dbPath, entries["app.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := app.Open(dbPath)
	if err != nil {
		t.Fatalf("restored app.db does not open as bbolt: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if _, found, err := restored.Get("app_backup1"); err != nil || !found {
		t.Errorf("restored app.db missing record (found=%v err=%v)", found, err)
	}

	var m backupManifest
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.CrucibleVersion == "" || m.CreatedAt.IsZero() {
		t.Errorf("manifest incomplete: %+v", m)
	}
	if len(m.Entries) != 3 { // app.db, tokens.json, registry-credentials.json
		t.Errorf("manifest entries = %v, want 3", m.Entries)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A bolt sanity guard: the streamed copy is byte-consistent (openable) even
// while the source db keeps serving — the read transaction pins the snapshot.
func TestAppStoreBackupToPinsConsistentCopy(t *testing.T) {
	dir := t.TempDir()
	store, err := app.Open(filepath.Join(dir, "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Put(app.Record{ID: "a1", Spec: api.AppSpec{Name: "one"}}); err != nil {
		t.Fatal(err)
	}

	out, err := os.Create(filepath.Join(dir, "copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	var promised int64
	err = store.BackupTo(func(size int64) (io.Writer, error) {
		promised = size
		return out, nil
	})
	if err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	_ = out.Close()
	fi, _ := os.Stat(out.Name())
	if fi.Size() != promised {
		t.Fatalf("wrote %d bytes, frame was promised %d", fi.Size(), promised)
	}
	copyDB, err := bolt.Open(out.Name(), 0o600, nil)
	if err != nil {
		t.Fatalf("copy does not open as bbolt: %v", err)
	}
	_ = copyDB.Close()
}
