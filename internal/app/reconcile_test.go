package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// fakeInstantiator records Create/Destroy and lets a test flip an
// instance's liveness to simulate a crash.
type fakeInstantiator struct {
	mu          sync.Mutex
	next        int
	live        map[string]string // instanceID -> appID
	creates     []string          // appIDs, in order
	destroys    []string          // instanceIDs, in order
	createErr   error
	probe       Health           // result Probe returns for live instances
	imageHealth *api.HealthCheck // what ImageHealth returns (nil = image has none)
	slept       map[string]bool  // instanceIDs currently asleep
	onSleep     func()           // if set, called (outside f.mu) mid-Sleep — a test gate
	snapshots   int              // count of SnapshotInstance calls (golden captures)
	forks       int              // count of ForkInstance calls (warm extras)
}

func (f *fakeInstantiator) SnapshotInstance(_ context.Context, instanceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[instanceID]; !ok {
		return "", errors.New("fake: no such instance")
	}
	f.snapshots++
	f.next++
	return "golden_" + string(rune('a'+f.next)), nil
}

func (f *fakeInstantiator) ForkInstance(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.forks++
	f.next++
	id := "sbx_fork_" + string(rune('a'+f.next))
	f.live[id] = "forked"
	return id, nil
}

func (f *fakeInstantiator) DeleteSnapshot(_ context.Context, _ string) error { return nil }

func (f *fakeInstantiator) ImageHealth(_ context.Context, _ api.AppSpec) (*api.HealthCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageHealth, nil
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

func (f *fakeInstantiator) Probe(_ context.Context, _ string, _ api.HealthCheck) Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probe
}

func (f *fakeInstantiator) setProbe(h Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe = h
}

func (f *fakeInstantiator) Destroy(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, instanceID)
	f.destroys = append(f.destroys, instanceID)
	return nil
}

// Sleep keeps the instance live (its record survives) but marks it asleep. If
// onSleep is set it runs mid-flight (outside f.mu) so a test can hold the sleep
// open and race a concurrent wake against it.
func (f *fakeInstantiator) SnapshotExists(string) bool { return true }

func (f *fakeInstantiator) Sleep(_ context.Context, instanceID string) (string, error) {
	f.mu.Lock()
	_, ok := f.live[instanceID]
	hook := f.onSleep
	f.mu.Unlock()
	if !ok {
		return "", errors.New("fake: no such instance")
	}
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.slept == nil {
		f.slept = map[string]bool{}
	}
	f.slept[instanceID] = true
	return "snap_" + instanceID, nil
}

func (f *fakeInstantiator) Wake(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.slept[instanceID] {
		return errors.New("fake: not asleep")
	}
	delete(f.slept, instanceID)
	return nil
}

func (f *fakeInstantiator) WakeFromSnapshot(_ context.Context, _ string, _ api.AppSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := "sbx_woken_" + string(rune('a'+f.next))
	f.live[id] = "woken"
	return id, nil
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

func ctx() context.Context { return context.Background() }

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

// fakeClock is a manually-advanced clock for deterministic backoff/health tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
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

	t.Run("always restarts (after backoff)", func(t *testing.T) {
		f := newFake()
		m, _ := newMgr(t, f)
		clk := &fakeClock{t: time.Unix(0, 0).UTC()}
		m.now = clk.now
		rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)
		m.reconcile(ctx)
		f.crash(rec.ID)
		m.reconcile(ctx) // records failure + schedules backoff, no reboot yet
		if f.liveCount() != 0 {
			t.Fatalf("rebooted during backoff: live=%d", f.liveCount())
		}
		clk.advance(2 * time.Second) // past baseBackoff (1s)
		m.reconcile(ctx)
		if f.liveCount() != 1 || f.createCount() != 2 {
			t.Fatalf("after backoff: live=%d create=%d, want 1/2", f.liveCount(), f.createCount())
		}
		if resp, _ := m.Get(rec.ID); resp.Status.Restarts != 1 {
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

func healthSpec(name string) api.AppSpec {
	s := nginxSpec(name, wire.RestartAlways)
	s.Health = &api.HealthCheck{Type: "http", Path: "/", Port: 80,
		IntervalSec: 5, UnhealthyThreshold: 3, HealthyThreshold: 1, StartPeriodSec: 5}
	return s
}

// TestBackoffIsExponential: repeated fast crashes push the reboot delay up
// baseBackoff·2^(n-1), and reboots only happen once the delay elapses.
func TestBackoffIsExponential(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	m.now = clk.now
	rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)

	prevCreates := 0
	// Each cycle: boot, crash immediately, expect the backoff to double.
	wantDelays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for i, want := range wantDelays {
		m.reconcile(ctx()) // boots
		if f.createCount() != prevCreates+1 {
			t.Fatalf("cycle %d: expected a boot", i)
		}
		prevCreates = f.createCount()
		f.crash(rec.ID)
		m.reconcile(ctx()) // records failure, schedules backoff
		// Just before the delay: no reboot.
		clk.advance(want - 100*time.Millisecond)
		m.reconcile(ctx())
		if f.createCount() != prevCreates {
			t.Fatalf("cycle %d: rebooted before backoff %v elapsed", i, want)
		}
		// Just after: reboot.
		clk.advance(200 * time.Millisecond)
	}
}

// TestCrashLoopGuard: after crashLoopThreshold fast failures the phase is
// crashlooping and backoff is capped at maxBackoff (still retried).
func TestCrashLoopGuard(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	m.now = clk.now
	rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)

	for i := 0; i < crashLoopThreshold+1; i++ {
		m.reconcile(ctx())
		f.crash(rec.ID)
		m.reconcile(ctx())
		clk.advance(maxBackoff + time.Second) // always past backoff
	}
	resp, _ := m.Get(rec.ID)
	if resp.Status.Phase != "crashlooping" {
		t.Fatalf("phase = %q, want crashlooping after %d fast failures", resp.Status.Phase, crashLoopThreshold)
	}
	if resp.Status.Restarts < crashLoopThreshold {
		t.Errorf("restarts = %d, want >= %d", resp.Status.Restarts, crashLoopThreshold)
	}
}

// TestStableRunResetsCrashLoop: an instance that survives past the window
// clears the failure count, so a later one-off crash isn't a loop.
func TestStableRunResetsCrashLoop(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	m.now = clk.now
	rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)

	m.reconcile(ctx()) // boot
	// Two fast failures.
	for i := 0; i < 2; i++ {
		f.crash(rec.ID)
		m.reconcile(ctx())
		clk.advance(2 * time.Second)
		m.reconcile(ctx()) // reboot
	}
	// Now run healthy past the window → failures reset.
	clk.advance(crashLoopWindow + time.Second)
	m.reconcile(ctx())
	if resp, _ := m.Get(rec.ID); resp.Status.Phase != "running" {
		t.Fatalf("phase = %q, want running after a stable run", resp.Status.Phase)
	}
	// A crash now starts backoff from base again (failures were reset).
	f.crash(rec.ID)
	m.reconcile(ctx())
	if resp, _ := m.Get(rec.ID); resp.Status.Restarts == 0 {
		t.Error("expected a restart recorded")
	}
}

// TestHealthDrivesRestart: a passing probe → healthy; sustained failing
// probes past the threshold → the instance is destroyed and rebooted.
func TestHealthDrivesRestart(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthPassing)
	rec, _ := m.Create(healthSpec("web"), true)

	m.reconcile(ctx()) // boot
	clk.advance(10 * time.Second)
	m.reconcile(ctx()) // probe passes
	if resp, _ := m.Get(rec.ID); resp.Status.Health != "healthy" {
		t.Fatalf("health = %q, want healthy", resp.Status.Health)
	}

	// Now fail health. Needs UnhealthyThreshold (3) consecutive failing
	// probes, each an interval apart, before a restart.
	f.setProbe(HealthFailing)
	createsBefore := f.createCount()
	for i := 0; i < 3; i++ {
		clk.advance(6 * time.Second) // past interval (5s)
		m.reconcile(ctx())
	}
	// The 3rd failing probe destroys + records failure; advance past backoff to reboot.
	clk.advance(2 * time.Second)
	m.reconcile(ctx())
	if f.createCount() != createsBefore+1 {
		t.Fatalf("unhealthy instance not restarted: creates %d → %d", createsBefore, f.createCount())
	}
}

// TestStartPeriodGrace: failing probes during the start period don't count.
func TestStartPeriodGrace(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthFailing)
	rec, _ := m.Create(healthSpec("web"), true) // StartPeriodSec: 5

	m.reconcile(ctx()) // boot at t=100
	createsBefore := f.createCount()
	// Probe within the 5s start period: failing, but graced.
	clk.advance(3 * time.Second)
	m.reconcile(ctx())
	if resp, _ := m.Get(rec.ID); resp.Status.Health != "unknown" {
		t.Errorf("health during start period = %q, want unknown", resp.Status.Health)
	}
	if f.createCount() != createsBefore {
		t.Error("restarted during start-period grace")
	}
}

func TestUpdateBumpsGenerationAndReplacesSpec(t *testing.T) {
	m, _ := newMgr(t, newFake())
	mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)

	updated := nginxSpec("web", wire.RestartOnFailure)
	updated.MemoryMiB = 512
	rec, err := m.Update("web", updated)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if rec.Generation != 2 {
		t.Errorf("generation = %d, want 2 (a bump triggers redeploy)", rec.Generation)
	}
	if rec.Spec.Restart.Policy != wire.RestartOnFailure || rec.Spec.MemoryMiB != 512 {
		t.Errorf("spec not replaced: %+v", rec.Spec)
	}
	if !rec.DesiredRunning {
		t.Error("desired running should be retained across update")
	}

	// Name is immutable.
	if _, err := m.Update("web", nginxSpec("web2", wire.RestartAlways)); err == nil {
		t.Error("name change accepted; want an immutable-name error")
	}
	// Unknown app.
	if _, err := m.Update("nope", nginxSpec("nope", wire.RestartAlways)); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown err = %v, want ErrNotFound", err)
	}
}

// proxySpec is a proxy-fronted app (a Port, no host publish) — the shape that
// qualifies for a zero-downtime rolling update.
func proxySpec(name string) api.AppSpec {
	s := nginxSpec(name, wire.RestartAlways)
	s.Port = 80
	return s
}

func proxyHealthSpec(name string) api.AppSpec {
	s := healthSpec(name)
	s.Port = 80
	return s
}

func instanceOf(t *testing.T, m *Manager, appID string) string {
	t.Helper()
	resp, err := m.Get(appID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status == nil {
		return ""
	}
	return resp.Status.InstanceID
}

// TestRollingUpdateFlipsAndDrains: a proxy-fronted app updates zero-downtime —
// the incoming boots without flipping, the route flips only once it's ready,
// and the superseded instance is drained (kept alive) then reaped.
func TestRollingUpdateFlipsAndDrains(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthPassing)
	rec := mustCreate(t, m, proxyHealthSpec("web"), true)

	m.reconcile(ctx()) // boot generation 1
	old := instanceOf(t, m, rec.ID)
	if old == "" || f.liveCount() != 1 {
		t.Fatalf("boot: instance=%q live=%d", old, f.liveCount())
	}

	updated := proxyHealthSpec("web")
	updated.MemoryMiB = 512
	if _, err := m.Update("web", updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	m.reconcile(ctx()) // startRoll: boot incoming WITHOUT flipping
	if f.liveCount() != 2 {
		t.Fatalf("during roll live=%d, want 2 (old + incoming)", f.liveCount())
	}
	if got := instanceOf(t, m, rec.ID); got != old {
		t.Fatalf("flipped before the incoming was ready: current=%s old=%s", got, old)
	}

	m.reconcile(ctx()) // incoming readiness passes → flip
	neu := instanceOf(t, m, rec.ID)
	if neu == old || neu == "" {
		t.Fatalf("did not flip: current=%s old=%s", neu, old)
	}
	if !f.Exists(old) {
		t.Fatal("old instance destroyed immediately; want it kept alive to drain")
	}
	if f.liveCount() != 2 {
		t.Fatalf("just after flip live=%d, want 2 (new + draining old)", f.liveCount())
	}
	if resp, _ := m.Get(rec.ID); resp.Status.InstanceGeneration != 2 {
		t.Fatalf("instance_generation=%d, want 2 after flip", resp.Status.InstanceGeneration)
	}

	clk.advance(drainWindow + time.Second)
	m.reconcile(ctx()) // drain window elapsed → reap old
	if f.Exists(old) {
		t.Fatal("draining instance not reaped after the drain window")
	}
	if f.liveCount() != 1 {
		t.Fatalf("after drain live=%d, want 1", f.liveCount())
	}
}

// setupMidDrain boots an app, updates it, and drives the roll to the point where
// the OLD instance is draining (post-flip, within the drain window). Returns the
// manager, fake, clock, record, and the id of the now-draining old instance.
func setupMidDrain(t *testing.T) (*Manager, *fakeInstantiator, *fakeClock, Record, string) {
	t.Helper()
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthPassing)
	rec := mustCreate(t, m, proxyHealthSpec("web"), true)

	m.reconcile(ctx()) // boot gen 1
	old := instanceOf(t, m, rec.ID)

	upd := proxyHealthSpec("web")
	upd.MemoryMiB = 512
	if _, err := m.Update("web", upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m.reconcile(ctx()) // startRoll: boot incoming
	m.reconcile(ctx()) // readiness passes → flip; old now draining
	if f.liveCount() != 2 || !f.Exists(old) {
		t.Fatalf("mid-drain setup: live=%d oldExists=%v, want 2 + old draining", f.liveCount(), f.Exists(old))
	}
	return m, f, clk, rec, old
}

// TestDeleteMidDrainDestroysDrainingInstance: deleting an app while a rolling
// update's OLD instance is still draining must tear down BOTH the current and the
// draining instance. Regression: the teardown path used to destroy only the
// current instance, then forget the app — orphaning the draining VM forever.
func TestDeleteMidDrainDestroysDrainingInstance(t *testing.T) {
	m, f, _, rec, old := setupMidDrain(t)

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.reconcile(ctx())
	if f.Exists(old) {
		t.Fatal("draining instance orphaned by app delete")
	}
	if f.liveCount() != 0 {
		t.Fatalf("delete mid-drain leaked %d instance(s), want 0", f.liveCount())
	}
}

// TestSupersedingUpdateReapsPriorDrainingInstance: a second update that flips
// INSIDE the first update's drain window must destroy the first update's draining
// instance before taking the single drain slot. Regression: flip overwrote the
// slot unconditionally, orphaning the earlier old instance.
func TestSupersedingUpdateReapsPriorDrainingInstance(t *testing.T) {
	m, f, clk, rec, orig := setupMidDrain(t)
	instA := instanceOf(t, m, rec.ID) // the instance made current by the first update
	if instA == "" || instA == orig {
		t.Fatalf("bad setup: instA=%q orig=%q", instA, orig)
	}

	// A second update lands well inside the 10s drain window (orig not yet reaped).
	clk.advance(2 * time.Second)
	updB := proxyHealthSpec("web")
	updB.MemoryMiB = 256
	if _, err := m.Update("web", updB); err != nil {
		t.Fatalf("Update B: %v", err)
	}
	m.reconcile(ctx()) // startRoll B
	m.reconcile(ctx()) // flip B → instA now draining; orig must be destroyed here

	if f.Exists(orig) {
		t.Fatalf("prior draining instance %s orphaned by the superseding update", orig)
	}
	if !f.Exists(instA) {
		t.Fatal("first update's instance should now be the (new) draining one")
	}
	if f.liveCount() != 2 {
		t.Fatalf("after superseding flip live=%d, want 2 (new current + one draining)", f.liveCount())
	}
}

// TestSleepMidDrainReapsDrainingInstance: sleeping an app while a roll's old
// instance is still draining must destroy that instance — otherwise a live VM
// keeps running while the app is "asleep", defeating scale-to-zero.
func TestSleepMidDrainReapsDrainingInstance(t *testing.T) {
	m, f, _, _, old := setupMidDrain(t)

	if err := m.Sleep(ctx(), "web"); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if f.Exists(old) {
		t.Fatal("draining instance left running while the app is asleep")
	}
	if f.liveCount() != 1 {
		t.Fatalf("asleep app has live=%d, want 1 (only the snapshotted current)", f.liveCount())
	}
}

// TestRollingUpdateNoHealthTCPGate: an app with no health check still rolls,
// gating the flip on a TCP connect to its port.
func TestRollingUpdateNoHealthTCPGate(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthPassing) // the synthesized tcp readiness probe passes
	rec := mustCreate(t, m, proxySpec("web"), true)

	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := proxySpec("web")
	upd.MemoryMiB = 256
	if _, err := m.Update("web", upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m.reconcile(ctx()) // startRoll
	m.reconcile(ctx()) // tcp gate passes → flip
	if got := instanceOf(t, m, rec.ID); got == old || got == "" {
		t.Fatalf("no-health app did not flip on the tcp gate: current=%s old=%s", got, old)
	}
}

// TestFailedUpdateKeepsOldServing: an update whose new instance never becomes
// ready aborts on the rollout deadline, leaving the OLD instance serving.
func TestFailedUpdateKeepsOldServing(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	f.setProbe(HealthPassing)
	rec := mustCreate(t, m, proxyHealthSpec("web"), true)
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := proxyHealthSpec("web")
	upd.MemoryMiB = 512
	if _, err := m.Update("web", upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	f.setProbe(HealthFailing) // the incoming never passes its readiness gate
	m.reconcile(ctx())        // startRoll
	if f.liveCount() != 2 {
		t.Fatalf("during roll live=%d, want 2", f.liveCount())
	}

	clk.advance(rolloutTimeout + time.Second)
	m.reconcile(ctx()) // past the rollout deadline → abort

	if got := instanceOf(t, m, rec.ID); got != old {
		t.Fatalf("route flipped on a failed update: current=%s old=%s", got, old)
	}
	if !f.Exists(old) {
		t.Fatal("old instance destroyed on a failed update")
	}
	if f.liveCount() != 1 {
		t.Fatalf("after abort live=%d, want 1 (incoming destroyed, old serving)", f.liveCount())
	}
	resp, _ := m.Get(rec.ID)
	if resp.Status.LastError == "" {
		t.Error("failed update recorded no LastError")
	}
	if resp.Status.Phase != "running" {
		t.Errorf("phase=%q during failed update, want running (old still serving)", resp.Status.Phase)
	}
	if resp.Status.InstanceGeneration != 1 {
		t.Errorf("instance_generation=%d, want 1 (still serving the old spec)", resp.Status.InstanceGeneration)
	}

	// A failed roll backs off: the very next pass must not immediately re-roll.
	createsAfterAbort := f.createCount()
	m.reconcile(ctx())
	if f.createCount() != createsAfterAbort {
		t.Error("re-rolled immediately after a failed update; want backoff gating")
	}
	// Past the backoff, it retries.
	clk.advance(2 * maxBackoff)
	m.reconcile(ctx())
	if f.createCount() == createsAfterAbort {
		t.Error("did not retry the roll after backoff elapsed")
	}
}

// TestNonProxyAppRedeployDestroyThenBoot: an app without a proxy port can't run
// two instances at once, so its update stays the classic destroy-then-boot.
func TestNonProxyAppRedeployDestroyThenBoot(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Unix(100, 0).UTC()}
	m.now = clk.now
	rec := mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true) // no Port
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := nginxSpec("web", wire.RestartAlways)
	upd.MemoryMiB = 256
	if _, err := m.Update("web", upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m.reconcile(ctx()) // canRoll=false → destroy old + boot new in one pass
	neu := instanceOf(t, m, rec.ID)
	if neu == old || neu == "" {
		t.Fatalf("no redeploy: current=%s old=%s", neu, old)
	}
	if f.Exists(old) {
		t.Fatal("old instance not destroyed in destroy-then-boot")
	}
	if f.liveCount() != 1 {
		t.Fatalf("live=%d, want 1 (a non-proxy app never runs two instances)", f.liveCount())
	}
}

func TestSeedHealthFromImage(t *testing.T) {
	// No app health + image declares one → seeded and persisted at boot.
	f := newFake()
	f.imageHealth = &api.HealthCheck{Type: "exec", Cmd: []string{"/bin/sh", "-c", "true"}}
	m, s := newMgr(t, f)
	_, _ = m.Create(nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(context.Background())
	got, found, err := s.GetByName("web")
	if err != nil || !found {
		t.Fatalf("GetByName: found=%v err=%v", found, err)
	}
	if got.Spec.Health == nil || got.Spec.Health.Type != "exec" {
		t.Fatalf("image health not seeded: %+v", got.Spec.Health)
	}

	// Explicit app health is never overwritten by the image seed.
	f2 := newFake()
	f2.imageHealth = &api.HealthCheck{Type: "exec", Cmd: []string{"x"}}
	m2, s2 := newMgr(t, f2)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Health = &api.HealthCheck{Type: "tcp", Port: 5432}
	_, _ = m2.Create(spec, true)
	m2.reconcile(context.Background())
	got2, _, _ := s2.GetByName("web")
	if got2.Spec.Health == nil || got2.Spec.Health.Type != "tcp" {
		t.Errorf("explicit health overwritten by seed: %+v", got2.Spec.Health)
	}
}

func TestCanCall(t *testing.T) {
	m, _ := newMgr(t, newFake())
	web := nginxSpec("web", wire.RestartAlways)
	web.CanCall = []string{"backend"}
	if _, err := m.Create(web, true); err != nil {
		t.Fatalf("create web: %v", err)
	}
	if _, err := m.Create(nginxSpec("backend", wire.RestartAlways), true); err != nil {
		t.Fatalf("create backend: %v", err)
	}

	if !m.CanCall("web", "backend") {
		t.Error("web→backend should be allowed (granted)")
	}
	if m.CanCall("backend", "web") {
		t.Error("backend→web should be denied (no grant) — default-deny")
	}
	if !m.CanCall("web", "web") {
		t.Error("web→web (self) should be allowed")
	}
	if m.CanCall("web", "unknown") {
		t.Error("web→unknown (not granted) should be denied")
	}
	if m.CanCall("ghost", "backend") {
		t.Error("unknown caller should be denied")
	}
	if m.CanCall("", "backend") || m.CanCall("web", "") {
		t.Error("empty caller/target should be denied")
	}
}

func TestValidateSpecCanCall(t *testing.T) {
	base := nginxSpec("web", wire.RestartAlways)
	base.CanCall = []string{"Bad Name"}
	if err := validateSpec(base); err == nil {
		t.Error("can_call with an invalid app name should fail validation")
	}
	base.CanCall = []string{"web"} // self-reference
	if err := validateSpec(base); err == nil {
		t.Error("can_call listing the app itself should fail validation")
	}
	base.CanCall = []string{"backend", "cache"}
	if err := validateSpec(base); err != nil {
		t.Errorf("valid can_call targets should pass: %v", err)
	}
}

// crashInstance removes one instance without going through Destroy (a single
// replica's VM dying underneath the daemon).
func (f *fakeInstantiator) crashInstance(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
}

func TestConvergeReplicas(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	c := ctx()

	// min_scale=3 → converge to 3 instances (primary + 2 extras) in one pass.
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Sleep = &api.SleepPolicy{MinScale: 3}
	mustCreate(t, m, spec, true)
	m.reconcile(c)
	if f.liveCount() != 3 {
		t.Fatalf("min_scale=3: %d live, want 3", f.liveCount())
	}
	resp, _ := m.GetByName("web")
	if resp.Status.Replicas != 3 || resp.Status.ReadyReplicas != 3 || len(resp.Status.Instances) != 3 {
		t.Errorf("status = replicas:%d ready:%d instances:%d, want 3/3/3",
			resp.Status.Replicas, resp.Status.ReadyReplicas, len(resp.Status.Instances))
	}

	// Kill one extra → the reconciler re-converges to 3.
	primary := resp.Status.InstanceID
	var victim string
	for _, in := range resp.Status.Instances {
		if in.InstanceID != primary {
			victim = in.InstanceID
			break
		}
	}
	f.crashInstance(victim)
	if f.liveCount() != 2 {
		t.Fatalf("after crashing an extra: %d live, want 2", f.liveCount())
	}
	m.reconcile(c)
	if f.liveCount() != 3 {
		t.Fatalf("after re-converge: %d live, want 3", f.liveCount())
	}

	// Scale down to min_scale=1 → destroys the 2 extras.
	spec2 := nginxSpec("web", wire.RestartAlways)
	spec2.Sleep = &api.SleepPolicy{MinScale: 1}
	if _, err := m.Update("web", spec2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m.reconcile(c)
	if f.liveCount() != 1 {
		t.Fatalf("after scale-down to min_scale=1: %d live, want 1", f.liveCount())
	}

	// A single-instance app never grows extras (N=1 path unchanged).
	f2 := newFake()
	m2, _ := newMgr(t, f2)
	mustCreate(t, m2, nginxSpec("solo", wire.RestartAlways), true)
	m2.reconcile(c)
	m2.reconcile(c)
	if f2.liveCount() != 1 {
		t.Fatalf("single-instance app: %d live, want 1", f2.liveCount())
	}
}

func TestExtrasForkFromGolden(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Sleep = &api.SleepPolicy{MinScale: 3}
	mustCreate(t, m, spec, true)
	m.reconcile(ctx())
	if f.liveCount() != 3 {
		t.Fatalf("%d live, want 3", f.liveCount())
	}
	if f.snapshots < 1 {
		t.Error("expected a golden snapshot captured for scale-out, got 0")
	}
	if f.forks != 2 {
		t.Errorf("expected 2 warm-forked extras, got %d", f.forks)
	}
	// Only the primary is a cold create; the extras are warm forks, not creates.
	if len(f.creates) != 1 {
		t.Errorf("expected 1 cold create (the primary), got %d", len(f.creates))
	}
}

func TestAutoscale(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	clk := &fakeClock{t: time.Now()}
	m.now = clk.now

	spec := nginxSpec("web", wire.RestartAlways)
	spec.Sleep = &api.SleepPolicy{MaxScale: 4, TargetConcurrency: 10}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx()) // primary running; scaleTarget floors at 1

	// ~35 concurrent requests → ceil(35/10) = 4 replicas (capped at max_scale).
	m.SetActivitySource(fakeActivity{"web": {inflight: 35}})
	for i := 0; i < 12; i++ {
		m.autoscaleCheck(clk.now())
	}
	if got := m.replicaTarget(rec); got != 4 {
		t.Errorf("under load: replicaTarget = %d, want 4", got)
	}

	// Load drops to zero: the stabilization window blocks an immediate scale-down.
	m.SetActivitySource(fakeActivity{"web": {inflight: 0}})
	m.autoscaleCheck(clk.now())
	if got := m.replicaTarget(rec); got != 4 {
		t.Errorf("right after load drop: replicaTarget = %d, want 4 (stabilization holds)", got)
	}

	// Past the stabilization window, the slow EWMA decays and it scales to the floor.
	clk.advance(2 * autoscaleDownStabilize)
	for i := 0; i < 40; i++ {
		m.autoscaleCheck(clk.now())
	}
	if got := m.replicaTarget(rec); got != 1 {
		t.Errorf("after calm: replicaTarget = %d, want 1 (scaled to floor)", got)
	}

	// A non-autoscaling app (no max_scale) ignores concurrency: static min_scale.
	f2 := newFake()
	m2, _ := newMgr(t, f2)
	rec2 := mustCreate(t, m2, sleepableSpec("solo", 0, 2), true) // min_scale 2, no max
	if got := m2.replicaTarget(rec2); got != 2 {
		t.Errorf("static app: replicaTarget = %d, want 2 (min_scale)", got)
	}
}

// TestConvergeReplicasNegatives exercises the failure/teardown edges of the
// extra-replica converge path.
func TestConvergeReplicasNegatives(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)

	// Boot the primary, then make all further boots (extra forks/creates) fail:
	// the app keeps its primary and doesn't crash or thrash.
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Sleep = &api.SleepPolicy{MinScale: 3}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx()) // primary + tries to fork 2 extras
	f.mu.Lock()
	f.createErr = errors.New("boom") // fork/create now fails
	f.mu.Unlock()
	// Kill the extras; the reconciler tries and fails to replace them — no panic.
	f.crash("forked")
	m.reconcile(ctx())
	if got := m.replicaTarget(rec); got != 3 {
		t.Errorf("target should stay 3 despite boot failures, got %d", got)
	}

	// Recover: boots succeed again → re-converges to 3.
	f.mu.Lock()
	f.createErr = nil
	f.mu.Unlock()
	m.reconcile(ctx())
	if f.liveCount() != 3 {
		t.Errorf("after recovery: %d live, want 3", f.liveCount())
	}

	// Stop the app → all replicas (primary + extras) torn down.
	if err := m.SetDesired(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	m.reconcile(ctx())
	if f.liveCount() != 0 {
		t.Errorf("after stop: %d live, want 0", f.liveCount())
	}
}

// TestAutoscaleNegatives covers the autoscaler's guards: capping at max, never
// below the floor, ignoring a non-running app, and a nil activity source.
func TestAutoscaleNegatives(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)

	spec := nginxSpec("web", wire.RestartAlways)
	spec.Sleep = &api.SleepPolicy{MinScale: 2, MaxScale: 4, TargetConcurrency: 1}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())

	// Extreme concurrency is capped at max_scale, never higher.
	m.SetActivitySource(fakeActivity{"web": {inflight: 1000}})
	for i := 0; i < 15; i++ {
		m.autoscaleCheck(m.now())
	}
	if got := m.replicaTarget(rec); got != 4 {
		t.Errorf("under extreme load: target = %d, want 4 (capped at max_scale)", got)
	}

	// Zero load never drops below the floor (min_scale 2).
	m.SetActivitySource(fakeActivity{"web": {inflight: 0}})
	clk := &fakeClock{t: time.Now().Add(time.Hour)} // past any stabilization
	m.now = clk.now
	for i := 0; i < 50; i++ {
		m.autoscaleCheck(clk.now())
	}
	if got := m.replicaTarget(rec); got != 2 {
		t.Errorf("idle: target = %d, want 2 (min_scale floor)", got)
	}

	// A nil activity source is a no-op (no panic, no scaling).
	m.activity = nil
	m.autoscaleCheck(m.now())
}
