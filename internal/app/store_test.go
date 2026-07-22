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

// PruneUsage reclaims a deleted app's retained counters so the bucket does not grow with
// apps-ever-created. Its safety property is the negative case: a LIVE app's record is its
// running counter, so pruning one would silently reset that app's lifetime totals.
func TestPruneUsage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	now := time.Now()
	old := now.Add(-90 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	put := func(id string, finalized *time.Time) {
		t.Helper()
		if err := s.PutUsage(id, Usage{AppID: id, FinalizedAt: finalized}); err != nil {
			t.Fatal(err)
		}
	}
	put("live-old", nil)        // live, ancient — must survive
	put("live-new", nil)        // live, fresh   — must survive
	put("done-old", &old)       // finalized, past cutoff — reclaim
	put("done-recent", &recent) // finalized, inside cutoff — keep

	cutoff := now.Add(-30 * 24 * time.Hour)
	n, err := s.PruneUsage(cutoff)
	if err != nil {
		t.Fatalf("PruneUsage: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}

	got, err := s.ListUsage()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live-old", "live-new", "done-recent"} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s was reclaimed and must not have been", id)
		}
	}
	if _, ok := got["done-old"]; ok {
		t.Error("done-old past the cutoff was not reclaimed")
	}

	// Idempotent: a second sweep finds nothing new.
	if n, err := s.PruneUsage(cutoff); err != nil || n != 0 {
		t.Errorf("second prune = %d, %v; want 0, nil", n, err)
	}
}

// A live record must survive any cutoff, including one in the future — age is never the
// deciding factor, FinalizedAt is.
func TestPruneUsageNeverTouchesLiveRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.PutUsage("live", Usage{AppID: "live"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneUsage(time.Now().Add(100 * 365 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d live records; must never reclaim a live app's counter", n)
	}
	if got, _ := s.ListUsage(); len(got) != 1 {
		t.Errorf("live record lost: %v", got)
	}
}

func TestPruneUsageEmptyBucket(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if n, err := s.PruneUsage(time.Now()); err != nil || n != 0 {
		t.Errorf("empty bucket prune = %d, %v; want 0, nil", n, err)
	}
}
