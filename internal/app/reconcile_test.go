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
}

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

// Sleep keeps the instance live (its record survives) but marks it asleep.
func (f *fakeInstantiator) Sleep(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[instanceID]; !ok {
		return errors.New("fake: no such instance")
	}
	if f.slept == nil {
		f.slept = map[string]bool{}
	}
	f.slept[instanceID] = true
	return nil
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
