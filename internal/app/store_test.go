package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func testRecord(id, name string) Record {
	return Record{
		ID: id,
		Spec: api.AppSpec{
			Name:      name,
			Image:     &api.ImageRef{OCI: "nginx:alpine"},
			VCPUs:     1,
			MemoryMiB: 256,
			Publish:   []api.PortMapping{{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}},
			Restart:   wire.RestartPolicy{Policy: wire.RestartAlways},
			Health:    &api.HealthCheck{Type: "http", Path: "/", Port: 80},
		},
		DesiredRunning: true,
		Generation:     1,
		CreatedAt:      time.Unix(1000, 0).UTC(),
		UpdatedAt:      time.Unix(1000, 0).UTC(),
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := testRecord("app_aaaa", "web")
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := s.Get("app_aaaa")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.Spec.Name != "web" || got.Spec.Image.OCI != "nginx:alpine" ||
		got.Spec.Restart.Policy != wire.RestartAlways || got.Spec.Health.Type != "http" ||
		!got.DesiredRunning || got.Generation != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestGetMissing(t *testing.T) {
	s := openTemp(t)
	_, found, err := s.Get("app_nope")
	if err != nil || found {
		t.Fatalf("missing app: found=%v err=%v", found, err)
	}
}

func TestPutUpsertsAndListDelete(t *testing.T) {
	s := openTemp(t)
	_ = s.Put(testRecord("app_a", "alpha"))
	_ = s.Put(testRecord("app_b", "beta"))

	// Upsert: same id, bumped generation.
	rec := testRecord("app_a", "alpha")
	rec.Generation = 2
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d apps, want 2", len(list))
	}
	got, _, _ := s.Get("app_a")
	if got.Generation != 2 {
		t.Errorf("upsert generation = %d, want 2", got.Generation)
	}

	if err := s.Delete("app_a"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Get("app_a"); found {
		t.Error("app_a present after delete")
	}
	if err := s.Delete("app_gone"); err != nil {
		t.Errorf("deleting absent id: %v", err)
	}
}

func TestGetByName(t *testing.T) {
	s := openTemp(t)
	_ = s.Put(testRecord("app_a", "alpha"))
	_ = s.Put(testRecord("app_b", "beta"))
	got, found, err := s.GetByName("beta")
	if err != nil || !found || got.ID != "app_b" {
		t.Fatalf("GetByName(beta) = %+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := s.GetByName("ghost"); found {
		t.Error("GetByName found a nonexistent name")
	}
}

// TestPersistsAcrossReopen is the point of the whole store: desired state
// must outlive the process (the daemon), so the reconciler can re-create
// the app after a restart.
func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apps.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Put(testRecord("app_x", "survivor")); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, found, err := s2.Get("app_x")
	if err != nil || !found || got.Spec.Name != "survivor" {
		t.Fatalf("after reopen: %+v found=%v err=%v", got, found, err)
	}
}
