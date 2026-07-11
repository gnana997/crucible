package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
)

// newBareManager builds a Manager with a stub runner — enough to
// satisfy daemon.New for tests that exercise only the image routes.
func newBareManager(t *testing.T) *sandbox.Manager {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{t: t},
		WorkBase: t.TempDir(),
		Kernel:   "/fake/vmlinux",
		Rootfs:   tmpl,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	return mgr
}

// fakeImageStore is a hermetic ImageStore — no registry, no mkfs.
type fakeImageStore struct {
	mu         sync.Mutex
	images     map[string]*oci.ImageRecord
	pullErr    error
	pullRootfs string // RootfsPath stamped on a pulled record (create needs it)
	lastPull   string
	imported   []byte
}

func newFakeImageStore() *fakeImageStore {
	return &fakeImageStore{images: map[string]*oci.ImageRecord{}}
}

func (f *fakeImageStore) Pull(_ context.Context, ref string) (*oci.ImageRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPull = ref
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	rec := &oci.ImageRecord{
		Digest:      "sha256:" + strings.Repeat("a", 64),
		SourceRef:   ref,
		SizeBytes:   512 << 20,
		ConvertMode: "pipe",
		RootfsPath:  f.pullRootfs,
		RunConfig:   &oci.RunConfig{Entrypoint: []string{"/app"}, Cmd: []string{"--serve"}},
	}
	f.images[rec.Digest] = rec
	return rec, nil
}

func (f *fakeImageStore) Import(_ context.Context, r io.Reader, _ string) (*oci.ImageRecord, error) {
	data, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imported = data
	rec := &oci.ImageRecord{Digest: "sha256:" + strings.Repeat("b", 64), SourceRef: "docker-archive"}
	f.images[rec.Digest] = rec
	return rec, nil
}

func (f *fakeImageStore) List() []*oci.ImageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*oci.ImageRecord, 0, len(f.images))
	for _, r := range f.images {
		out = append(out, r)
	}
	return out
}

func (f *fakeImageStore) Get(ref string) (*oci.ImageRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.images[ref]; ok {
		return r, nil
	}
	return nil, oci.ErrImageNotFound
}

// seed adds a record resolvable by the given ref, with a real rootfs
// path so a create-from-image clone succeeds.
func (f *fakeImageStore) seed(ref, rootfsPath string) *oci.ImageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := &oci.ImageRecord{Digest: ref, SourceRef: ref, RootfsPath: rootfsPath}
	f.images[ref] = rec
	return rec
}

func (f *fakeImageStore) Delete(ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[ref]; !ok {
		return oci.ErrImageNotFound
	}
	delete(f.images, ref)
	return nil
}

func newImageTestServer(t *testing.T, store ImageStore) *httptest.Server {
	t.Helper()
	srv, err := New(Config{
		Manager: newBareManager(t),
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Images:  store,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestImageRoutesPullAndList(t *testing.T) {
	store := newFakeImageStore()
	ts := newImageTestServer(t, store)

	resp := serviceReq(t, http.MethodPost, ts.URL+"/images", `{"ref":"nginx:latest"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pull = %d: %s", resp.StatusCode, b)
	}
	var img api.ImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&img); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.SourceRef != "nginx:latest" || img.Entrypoint[0] != "/app" {
		t.Errorf("pull response: %+v", img)
	}
	if store.lastPull != "nginx:latest" {
		t.Errorf("store saw ref %q", store.lastPull)
	}

	resp = serviceReq(t, http.MethodGet, ts.URL+"/images", "")
	var list api.ImageListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Images) != 1 {
		t.Fatalf("list = %d images, want 1", len(list.Images))
	}
}

func TestImageRoutesImport(t *testing.T) {
	store := newFakeImageStore()
	ts := newImageTestServer(t, store)

	body := bytes.NewBufferString("FAKE-DOCKER-SAVE-TAR")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/images/import?tag=x:y", body)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import = %d", resp.StatusCode)
	}
	if string(store.imported) != "FAKE-DOCKER-SAVE-TAR" {
		t.Errorf("store imported %q", store.imported)
	}
}

func TestImageRoutesGetAndDelete(t *testing.T) {
	store := newFakeImageStore()
	ts := newImageTestServer(t, store)
	serviceReq(t, http.MethodPost, ts.URL+"/images", `{"ref":"redis:7"}`)
	digest := "sha256:" + strings.Repeat("a", 64)

	resp := serviceReq(t, http.MethodGet, ts.URL+"/images/"+digest, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	resp = serviceReq(t, http.MethodDelete, ts.URL+"/images/"+digest, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp = serviceReq(t, http.MethodGet, ts.URL+"/images/"+digest, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
}

func TestImageRoutesErrors(t *testing.T) {
	store := newFakeImageStore()
	ts := newImageTestServer(t, store)

	if resp := serviceReq(t, http.MethodPost, ts.URL+"/images", `{"ref":""}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty ref = %d, want 400", resp.StatusCode)
	}
	if resp := serviceReq(t, http.MethodPost, ts.URL+"/images", `{bad json`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json = %d, want 400", resp.StatusCode)
	}
	if resp := serviceReq(t, http.MethodGet, ts.URL+"/images/sha256:missing", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing = %d, want 404", resp.StatusCode)
	}

	store.pullErr = errors.New("upstream unreachable")
	if resp := serviceReq(t, http.MethodPost, ts.URL+"/images", `{"ref":"x"}`); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("pull failure = %d, want 502", resp.StatusCode)
	}
}

func TestImageRoutesDisabledWhenNoStore(t *testing.T) {
	ts := newImageTestServer(t, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/images", `{"ref":"x"}`},
		{http.MethodGet, "/images", ""},
		{http.MethodDelete, "/images/sha256:x", ""},
	} {
		resp := serviceReq(t, tc.method, ts.URL+tc.path, tc.body)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s with no store = %d, want 501", tc.method, tc.path, resp.StatusCode)
		}
	}
}
