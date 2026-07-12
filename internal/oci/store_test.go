package oci

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func storeRequires(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"mkfs.ext4", "fsck.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed; skipping store test", tool)
		}
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	mode := ModeStaging
	if ProbeTarballSupport(t.Context()) {
		mode = ModePipe
	}
	s, err := New(StoreConfig{
		Dir:         dir,
		Agent:       []byte("FAKE-AGENT-BINARY"),
		Mode:        mode,
		PullOptions: []PullOption{WithInsecureRegistry()},
	})
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	return s, dir
}

func TestStorePullConvertAndDedup(t *testing.T) {
	storeRequires(t)
	reg := newFakeRegistry(t)
	img := craftImage(t, "linux", "amd64", v1.Config{Entrypoint: []string{"/app"}})
	ref := reg + "/apps/web:v1"
	pushImage(t, ref, img)

	s, dir := newTestStore(t)
	rec, err := s.Pull(t.Context(), ref, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if rec.Digest != mustDigest(t, img) {
		t.Errorf("digest = %s, want %s", rec.Digest, mustDigest(t, img))
	}
	if _, err := os.Stat(rec.RootfsPath); err != nil {
		t.Errorf("rootfs not on disk: %v", err)
	}
	if !strings.HasPrefix(rec.RootfsPath, dir) {
		t.Errorf("rootfs %q not under store dir %q", rec.RootfsPath, dir)
	}
	if rec.RunConfig == nil || rec.RunConfig.Entrypoint[0] != "/app" {
		t.Errorf("run config not recorded: %+v", rec.RunConfig)
	}

	// Second pull dedupes: same record, no second artifact dir.
	rec2, err := s.Pull(t.Context(), ref, nil)
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if rec2.RootfsPath != rec.RootfsPath {
		t.Errorf("dedup failed: %q vs %q", rec2.RootfsPath, rec.RootfsPath)
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("List has %d images after dedup, want 1", got)
	}
}

func TestStoreImport(t *testing.T) {
	storeRequires(t)
	img := craftImage(t, "linux", "amd64", v1.Config{Cmd: []string{"/bin/sh"}})
	tag, _ := name.NewTag("example.com/side/load:v1")
	archive := filepath.Join(t.TempDir(), "img.tar")
	if err := tarball.WriteToFile(archive, tag, img); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}

	s, _ := newTestStore(t)
	rec, err := s.Import(t.Context(), bytes.NewReader(data), "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rec.SourceRef != "docker-archive" || rec.RunConfig.Cmd[0] != "/bin/sh" {
		t.Errorf("imported record: %+v", rec)
	}
	if _, err := os.Stat(rec.RootfsPath); err != nil {
		t.Errorf("imported rootfs missing: %v", err)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	storeRequires(t)
	reg := newFakeRegistry(t)
	img := craftImage(t, "linux", "amd64", v1.Config{})
	ref := reg + "/apps/persist:v1"
	pushImage(t, ref, img)

	s, dir := newTestStore(t)
	rec, err := s.Pull(t.Context(), ref, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Reopen the same dir: the scan must re-adopt the image.
	s2, err := New(StoreConfig{Dir: dir, Agent: []byte("A"), Mode: s.mode})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(rec.Digest)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Digest != rec.Digest || got.RunConfig == nil {
		t.Errorf("re-adopted record incomplete: %+v", got)
	}
	if _, err := os.Stat(got.RootfsPath); err != nil {
		t.Errorf("re-adopted rootfs path invalid: %v", err)
	}
}

func TestStoreScanSkipsIncompleteDirs(t *testing.T) {
	dir := t.TempDir()
	// A digest dir with a record but no rootfs must be skipped, not fatal.
	bad := filepath.Join(dir, "sha256_deadbeef")
	if err := os.MkdirAll(bad, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, recordName), []byte(`{"digest":"sha256:deadbeef"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := New(StoreConfig{Dir: dir, Agent: []byte("A"), Mode: ModeStaging})
	if err != nil {
		t.Fatalf("New with incomplete dir: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("incomplete dir was indexed: %d images", got)
	}
}

func TestStoreGetResolution(t *testing.T) {
	storeRequires(t)
	reg := newFakeRegistry(t)
	img := craftImage(t, "linux", "amd64", v1.Config{})
	ref := reg + "/apps/resolve:v1"
	pushImage(t, ref, img)

	s, _ := newTestStore(t)
	rec, err := s.Pull(t.Context(), ref, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	hex := strings.TrimPrefix(rec.Digest, "sha256:")

	for _, q := range []string{rec.Digest, hex, hex[:12], ref} {
		if got, err := s.Get(q); err != nil || got.Digest != rec.Digest {
			t.Errorf("Get(%q) = %v, %v", q, got, err)
		}
	}
	if _, err := s.Get("sha256:nope"); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrImageNotFound", err)
	}
}

func TestStoreDelete(t *testing.T) {
	storeRequires(t)
	reg := newFakeRegistry(t)
	img := craftImage(t, "linux", "amd64", v1.Config{})
	ref := reg + "/apps/del:v1"
	pushImage(t, ref, img)

	s, _ := newTestStore(t)
	rec, err := s.Pull(t.Context(), ref, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	dir := filepath.Dir(rec.RootfsPath)

	if err := s.Delete(rec.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("image dir survived delete: %v", err)
	}
	if _, err := s.Get(rec.Digest); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("deleted image still resolvable: %v", err)
	}
	if err := s.Delete(rec.Digest); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("double delete = %v, want ErrImageNotFound", err)
	}
}

func TestStoreRequiresAgent(t *testing.T) {
	if _, err := New(StoreConfig{Dir: t.TempDir(), Mode: ModeStaging}); err == nil {
		t.Fatal("New without an agent accepted")
	}
}

func TestDigestDir(t *testing.T) {
	if got := digestDir("sha256:abc123"); got != "sha256_abc123" {
		t.Errorf("digestDir = %q, want sha256_abc123", got)
	}
}

var _ = context.Background
