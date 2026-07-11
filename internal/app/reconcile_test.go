package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// fakeInstantiator records Create/Destroy and lets a test flip an
// instance's liveness to simulate a crash.
type fakeInstantiator struct {
	mu        sync.Mutex
	next      int
	live      map[string]string // instanceID -> appID
	creates   []string          // appIDs, in order
	destroys  []string          // instanceIDs, in order
	createErr error
}

func mustCreate(t *testing.T, m *Manager, spec api.AppSpec, running bool) Record {
	t.Helper()
	rec, err := m.Create(spec, running)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return rec
}

func newFake() *fakeInstantiator { return &fakeInstantiator{live: map[string]string{}} }

func (f *fakeInstantiator) Create(_ context.Context, appID string, _ api.AppSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.next++
	id := "sbx_" + appID + "_" + string(rune('a'+f.next))
	f.live[id] = appID
	f.creates = append(f.creates, appID)
	return id, nil
}

func (f *fakeInstantiator) Exists(instanceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.live[instanceID]
	return ok
}

func (f *fakeInstantiator) Destroy(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, instanceID)
	f.destroys = append(f.destroys, instanceID)
	return nil
}

// crash removes the app's live instance without going through Destroy,
// simulating a VM dying underneath the daemon.
func (f *fakeInstantiator) crash(appID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, a := range f.live {
		if a == appID {
			delete(f.live, id)
		}
	}
}

func (f *fakeInstantiator) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeInstantiator) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newMgr(t *testing.T, f Instantiator) (*Manager, *Store) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewManager(s, f, quietLog()), s
}

func nginxSpec(name string, policy string) api.AppSpec {
	return api.AppSpec{
		Name:    name,
		Image:   &api.ImageRef{OCI: "nginx:alpine"},
		Restart: wire.RestartPolicy{Policy: policy},
	}
}

func TestCreateValidatesAndPersists(t *testing.T) {
	m, _ := newMgr(t, newFake())

	if _, err := m.Create(api.AppSpec{Name: "Bad Name", Image: &api.ImageRef{OCI: "x"}}, true); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := m.Create(api.AppSpec{Name: "web"}, true); err == nil {
		t.Error("missing image accepted")
	}

	rec, err := m.Create(nginxSpec("web", wire.RestartAlways), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !IsValidID(rec.ID) || rec.Generation != 1 || !rec.DesiredRunning {
		t.Fatalf("bad record %+v", rec)
	}
	// Name uniqueness.
	if _, err := m.Create(nginxSpec("web", wire.RestartAlways), true); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name err = %v, want ErrNameTaken", err)
	}
}

func TestReconcileBootsAndTearsDown(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	ctx := context.Background()

	rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx)
	if f.liveCount() != 1 {
		t.Fatalf("after create+reconcile: %d live, want 1", f.liveCount())
	}
	resp, _ := m.Get(rec.ID)
	if resp.Status == nil || resp.Status.Phase != "running" || resp.Status.InstanceID == "" {
		t.Fatalf("status = %+v, want running with instance", resp.Status)
	}

	// Delete → reconcile tears the instance down.
	if err := m.Delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	m.reconcile(ctx)
	if f.liveCount() != 0 {
		t.Fatalf("after delete+reconcile: %d live, want 0", f.liveCount())
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)
	for i := 0; i < 5; i++ {
		m.reconcile(context.Background())
	}
	if f.createCount() != 1 || f.liveCount() != 1 {
		t.Fatalf("repeated reconcile created %d (want 1), live %d (want 1)", f.createCount(), f.liveCount())
	}
}

func TestRestartOnVanishRespectsPolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("always restarts", func(t *testing.T) {
		f := newFake()
		m, _ := newMgr(t, f)
		rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)
		m.reconcile(ctx)
		f.crash(rec.ID)
		m.reconcile(ctx)
		if f.liveCount() != 1 || f.createCount() != 2 {
			t.Fatalf("live=%d create=%d, want 1/2", f.liveCount(), f.createCount())
		}
		resp, _ := m.Get(rec.ID)
		if resp.Status.Restarts != 1 {
			t.Errorf("restarts = %d, want 1", resp.Status.Restarts)
		}
	})

	t.Run("never stays down", func(t *testing.T) {
		f := newFake()
		m, _ := newMgr(t, f)
		rec, _ := m.Create(nginxSpec("web", wire.RestartNever), true)
		m.reconcile(ctx)
		f.crash(rec.ID)
		m.reconcile(ctx)
		if f.liveCount() != 0 || f.createCount() != 1 {
			t.Fatalf("live=%d create=%d, want 0/1 (never restarts)", f.liveCount(), f.createCount())
		}
		resp, _ := m.Get(rec.ID)
		if resp.Status.Phase != "stopped" {
			t.Errorf("phase = %q, want stopped", resp.Status.Phase)
		}
	})
}

func TestSetDesiredStopsAndStarts(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	ctx := context.Background()
	rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx)

	if err := m.SetDesired(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	m.reconcile(ctx)
	if f.liveCount() != 0 {
		t.Fatalf("stopped app still has %d live", f.liveCount())
	}

	if err := m.SetDesired(rec.ID, true); err != nil {
		t.Fatal(err)
	}
	m.reconcile(ctx)
	if f.liveCount() != 1 {
		t.Fatalf("restarted app has %d live, want 1", f.liveCount())
	}
}

// TestSurvivesRestart is the headline: desired state persists, and a fresh
// Manager over the same store re-creates every desired-running app on its
// first reconcile — exactly the daemon-restart recovery path (old
// instances reaped, observed map empty, store intact).
func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apps.db")
	ctx := context.Background()

	// First daemon: create two running apps + one stopped.
	s1, _ := Open(path)
	f1 := newFake()
	m1 := NewManager(s1, f1, quietLog())
	mustCreate(t, m1, nginxSpec("web", wire.RestartAlways), true)
	mustCreate(t, m1, nginxSpec("api", wire.RestartAlways), true)
	mustCreate(t, m1, nginxSpec("worker", wire.RestartNever), false) // stopped
	m1.reconcile(ctx)
	if f1.liveCount() != 2 {
		t.Fatalf("first daemon: %d live, want 2", f1.liveCount())
	}
	_ = s1.Close()

	// Second daemon: brand-new Manager + Instantiator (old instances were
	// reaped by the sandbox layer), same store. First reconcile must
	// re-boot the two running apps and leave the stopped one down.
	s2, _ := Open(path)
	t.Cleanup(func() { _ = s2.Close() })
	f2 := newFake()
	m2 := NewManager(s2, f2, quietLog())
	if f2.liveCount() != 0 {
		t.Fatal("second daemon started with live instances")
	}
	m2.reconcile(ctx)
	if f2.liveCount() != 2 {
		t.Fatalf("after restart reconcile: %d live, want 2 (the running apps)", f2.liveCount())
	}
	if f2.createCount() != 2 {
		t.Fatalf("created %d, want exactly 2", f2.createCount())
	}
}

func TestStartRunsInitialReconcileThenStops(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)

	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Start's initial pass is synchronous, so the instance is up on return.
	if f.liveCount() != 1 {
		t.Fatalf("after Start: %d live, want 1", f.liveCount())
	}
	m.Stop() // must return (loop goroutine exits)
}
