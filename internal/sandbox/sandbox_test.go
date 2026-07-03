package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/runner"
)

// --- ID tests ---------------------------------------------------------

func TestNewIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if !strings.HasPrefix(id, "sbx_") {
			t.Fatalf("%q: missing sbx_ prefix", id)
		}
		// 8 random bytes -> 13 base32 chars unpadded.
		if want, got := len("sbx_")+13, len(id); got != want {
			t.Fatalf("%q: length %d, want %d", id, got, want)
		}
		if !IsValidID(id) {
			t.Fatalf("IsValidID(%q) = false", id)
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id after %d iterations: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestIsValidID(t *testing.T) {
	valid, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{valid, true},
		{"", false},
		{"sbx_", false},
		{"foo_abcdef", false},
		{"sbx_!!!!!!!!!", false},
	} {
		if got := IsValidID(tc.in); got != tc.want {
			t.Errorf("IsValidID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- Manager tests ----------------------------------------------------

// stubHandle is a runner.Handle for tests: records shutdown + wait calls
// and synthesizes a done channel.
type stubHandle struct {
	workdir  string
	vsock    string // agent UDS path; empty for cold-boot handles
	shutdown chan struct{}
	shutErr  error
}

func newStubHandle(workdir string) *stubHandle {
	return &stubHandle{workdir: workdir, shutdown: make(chan struct{})}
}

func (h *stubHandle) Workdir() string                               { return h.workdir }
func (h *stubHandle) VSockPath() string                             { return h.vsock }
func (h *stubHandle) Pause(context.Context) error                   { return nil }
func (h *stubHandle) Resume(context.Context) error                  { return nil }
func (h *stubHandle) PatchRootfs(_ context.Context, _ string) error { return nil }

// Snapshot writes empty state + memory files so that downstream Clone
// calls in Manager.Snapshot find real paths. Tests that want to inject
// failures here can extend stubHandle with an error-producing Snapshot
// method.
func (h *stubHandle) Snapshot(_ context.Context, statePath, memPath string) error {
	if err := os.WriteFile(statePath, []byte("stub-state"), 0o640); err != nil {
		return err
	}
	if err := os.WriteFile(memPath, []byte("stub-memory"), 0o640); err != nil {
		return err
	}
	return nil
}

func (h *stubHandle) Shutdown(ctx context.Context) error {
	select {
	case <-h.shutdown:
	default:
		close(h.shutdown)
	}
	return h.shutErr
}
func (h *stubHandle) Wait() error { <-h.shutdown; return nil }

// stubRunner is a runner.Runner for tests: records Start calls and
// produces stubHandles, optionally returning a pre-canned error.
type stubRunner struct {
	// t, when set, makes Restore stand up an in-process agent behind the
	// handle's vsock so the fork path exercises the real clone-safety
	// refresh (a fork with no agent channel is now fatal — see forkRestore).
	t *testing.T

	mu           sync.Mutex
	calls        []runner.Spec
	restoreCalls []runner.RestoreSpec
	startErr     error
	restoreErr   error
	handles      []*stubHandle

	// Concurrency probe for the fork fan-out cap. When restoreDelay > 0,
	// each Restore holds for that long (outside the mutex) while tracking
	// the peak number of simultaneous callers, so a test can assert Fork
	// never runs more forks at once than its concurrency limit.
	restoreDelay time.Duration
	inFlight     int32
	maxInFlight  int32
}

func (r *stubRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, spec)
	if r.startErr != nil {
		return nil, r.startErr
	}
	// Simulate the real runner creating the workdir so Delete's os.RemoveAll
	// actually has something to remove (and thus we cover that line too).
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *stubRunner) Restore(_ context.Context, spec runner.RestoreSpec) (runner.Handle, error) {
	if r.restoreDelay > 0 {
		cur := atomic.AddInt32(&r.inFlight, 1)
		for {
			peak := atomic.LoadInt32(&r.maxInFlight)
			if cur <= peak || atomic.CompareAndSwapInt32(&r.maxInFlight, peak, cur) {
				break
			}
		}
		time.Sleep(r.restoreDelay)
		atomic.AddInt32(&r.inFlight, -1)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restoreCalls = append(r.restoreCalls, spec)
	if r.restoreErr != nil {
		return nil, r.restoreErr
	}
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
	// Forks require an agent channel for clone-safety; serve a stub agent
	// on a per-fork socket so refreshIdentity (and any RefreshNetwork)
	// succeeds against the real code path.
	if r.t != nil {
		sock := filepath.Join(spec.Workdir, "a.sock")
		serveHybrid(r.t, sock, stubAgentHandler())
		h.vsock = sock
	}
	r.handles = append(r.handles, h)
	return h, nil
}

// newTestManager constructs a Manager backed by a fresh stubRunner.
// A tiny template rootfs file is created so Manager.Create's Clone call
// succeeds — the stub runner doesn't read it, but Clone expects the path
// to exist.
func newTestManager(t *testing.T) (*Manager, *stubRunner) {
	t.Helper()
	workBase := t.TempDir()

	tmplDir := t.TempDir()
	template := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake-template-bytes"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}

	r := &stubRunner{t: t}
	m, err := NewManager(ManagerConfig{
		Runner:   r,
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   template,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, r
}

func TestNewManagerValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  ManagerConfig
	}{
		{"no runner", ManagerConfig{WorkBase: "/tmp", Kernel: "k", Rootfs: "r"}},
		{"no workbase", ManagerConfig{Runner: &stubRunner{}, Kernel: "k", Rootfs: "r"}},
		{"no kernel", ManagerConfig{Runner: &stubRunner{}, WorkBase: "/tmp", Rootfs: "r"}},
		{"no rootfs", ManagerConfig{Runner: &stubRunner{}, WorkBase: "/tmp", Kernel: "k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewManager(tc.cfg); err == nil {
				t.Fatalf("NewManager(%s): got nil, want error", tc.name)
			}
		})
	}
}

func TestManagerCreateAppliesDefaults(t *testing.T) {
	m, r := newTestManager(t)

	s, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.VCPUs != DefaultVCPUs || s.MemoryMiB != DefaultMemoryMiB {
		t.Errorf("defaults not applied: VCPUs=%d MemoryMiB=%d", s.VCPUs, s.MemoryMiB)
	}
	if s.Workdir != filepath.Join(m.cfg.WorkBase, s.ID) {
		t.Errorf("Workdir = %q, want %q", s.Workdir, filepath.Join(m.cfg.WorkBase, s.ID))
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}

	// The per-sandbox rootfs clone should exist under the workdir.
	sbxRootfs := filepath.Join(s.Workdir, perSandboxRootfsName)
	if _, err := os.Stat(sbxRootfs); err != nil {
		t.Errorf("per-sandbox rootfs missing under workdir: %v", err)
	}

	// The runner should receive the *cloned* rootfs path, not the template.
	if len(r.calls) != 1 {
		t.Fatalf("runner.Start called %d times, want 1", len(r.calls))
	}
	got := r.calls[0]
	if got.Kernel != "/fake/vmlinux" {
		t.Errorf("Kernel = %q, want /fake/vmlinux", got.Kernel)
	}
	if got.Rootfs != sbxRootfs {
		t.Errorf("Rootfs = %q, want per-sandbox clone %q", got.Rootfs, sbxRootfs)
	}
	if got.Rootfs == m.cfg.Rootfs {
		t.Error("runner received template path; should be per-sandbox clone")
	}
	if got.VCPUs != DefaultVCPUs || got.MemoryMiB != DefaultMemoryMiB {
		t.Errorf("spec sizing wrong: %+v", got)
	}
}

func TestManagerGet(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(context.Background(), CreateConfig{VCPUs: 2, MemoryMiB: 1024})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.VCPUs != 2 || s.MemoryMiB != 1024 {
		t.Errorf("Create did not honor config: VCPUs=%d MemoryMiB=%d", s.VCPUs, s.MemoryMiB)
	}

	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", s.ID, err)
	}
	if got != s {
		t.Error("Get returned a different pointer than Create")
	}

	if _, err := m.Get("sbx_doesnotexist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown): err=%v, want ErrNotFound", err)
	}
}

func TestManagerListReturnsSnapshot(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < 3; i++ {
		if _, err := m.Create(context.Background(), CreateConfig{}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	list := m.List()
	if len(list) != 3 {
		t.Fatalf("List returned %d, want 3", len(list))
	}
}

func TestManagerDelete(t *testing.T) {
	m, r := newTestManager(t)
	s, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Workdir exists after Create (stub runner simulates that).
	if _, err := os.Stat(s.Workdir); err != nil {
		t.Fatalf("Workdir not present after Create: %v", err)
	}

	if err := m.Delete(context.Background(), s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Handle was shut down.
	select {
	case <-r.handles[0].shutdown:
	default:
		t.Error("Shutdown was not called on handle")
	}

	// Map no longer has the entry.
	if _, err := m.Get(s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err=%v, want ErrNotFound", err)
	}

	// Workdir removed.
	if _, err := os.Stat(s.Workdir); !os.IsNotExist(err) {
		t.Errorf("Workdir still exists after Delete: %v", err)
	}

	// Deleting again returns ErrNotFound.
	if err := m.Delete(context.Background(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete twice: err=%v, want ErrNotFound", err)
	}
}

func TestManagerCreateRunnerErrorCleansUp(t *testing.T) {
	wantErr := errors.New("start boom")
	workBase := t.TempDir()

	tmplDir := t.TempDir()
	template := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(template, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	r := &stubRunner{startErr: wantErr}
	m, err := NewManager(ManagerConfig{
		Runner:   r,
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   template,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := m.Create(context.Background(), CreateConfig{}); !errors.Is(err, wantErr) {
		t.Fatalf("Create: err=%v, want wraps %v", err, wantErr)
	}
	if len(m.List()) != 0 {
		t.Errorf("List returned %d entries, want 0", len(m.List()))
	}

	// Workdir + cloned rootfs should have been removed on rollback.
	entries, err := os.ReadDir(workBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("workbase still contains %v after failed Create; want empty", names)
	}
}

// --- Snapshot / Fork / DeleteSnapshot ---------------------------------

func TestManagerSnapshotAndFork(t *testing.T) {
	m, r := newTestManager(t)

	source, err := m.Create(context.Background(), CreateConfig{VCPUs: 2, MemoryMiB: 256})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}

	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.HasPrefix(snap.ID, "snap_") {
		t.Errorf("snap id %q: missing snap_ prefix", snap.ID)
	}
	if snap.SourceID != source.ID {
		t.Errorf("SourceID = %q, want %q", snap.SourceID, source.ID)
	}
	if snap.VCPUs != 2 || snap.MemoryMiB != 256 {
		t.Errorf("snap sizing = %d/%d, want 2/256", snap.VCPUs, snap.MemoryMiB)
	}
	// Snapshot files should exist on disk.
	for _, p := range []string{snap.StatePath, snap.MemPath, snap.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("snapshot file missing: %v", err)
		}
	}
	// Source sandbox must still be registered + live.
	if _, err := m.Get(source.ID); err != nil {
		t.Errorf("source gone after snapshot: %v", err)
	}

	forks, err := m.Fork(context.Background(), snap.ID, 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(forks) != 3 {
		t.Fatalf("len(forks) = %d, want 3", len(forks))
	}

	// Each fork should have its own workdir + its own rootfs clone.
	// Guest memory is NOT cloned per fork: the restore spec must point
	// at the snapshot's memory file and request the lazy uffd path.
	for _, f := range forks {
		if !strings.HasPrefix(f.ID, "sbx_") {
			t.Errorf("fork id %q: missing sbx_ prefix", f.ID)
		}
		if f.VCPUs != snap.VCPUs || f.MemoryMiB != snap.MemoryMiB {
			t.Errorf("fork sizing = %d/%d, want %d/%d from snapshot",
				f.VCPUs, f.MemoryMiB, snap.VCPUs, snap.MemoryMiB)
		}
		if _, err := os.Stat(filepath.Join(f.Workdir, perSandboxRootfsName)); err != nil {
			t.Errorf("fork %s missing %s: %v", f.ID, perSandboxRootfsName, err)
		}
		if _, err := os.Stat(filepath.Join(f.Workdir, snapshotMemoryName)); err == nil {
			t.Errorf("fork %s has a per-fork memory clone; memory must be shared lazily", f.ID)
		}
	}

	// The runner should have seen three Restore calls, all sharing the
	// snapshot memory file read-only via the lazy path.
	if len(r.restoreCalls) != 3 {
		t.Errorf("runner.Restore called %d times, want 3", len(r.restoreCalls))
	}
	for _, spec := range r.restoreCalls {
		if !spec.LazyMem {
			t.Errorf("Restore spec LazyMem = false, want true")
		}
		if spec.MemPath != snap.MemPath {
			t.Errorf("Restore spec MemPath = %q, want shared snapshot mem %q", spec.MemPath, snap.MemPath)
		}
	}
}

func TestManagerDeleteSnapshot(t *testing.T) {
	m, _ := newTestManager(t)

	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := m.DeleteSnapshot(context.Background(), snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := m.GetSnapshot(snap.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("GetSnapshot after delete: err=%v, want ErrSnapshotNotFound", err)
	}
	if _, err := os.Stat(snap.Dir); !os.IsNotExist(err) {
		t.Errorf("snapshot dir still exists: %v", err)
	}

	// Idempotent: second delete returns ErrSnapshotNotFound.
	if err := m.DeleteSnapshot(context.Background(), snap.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("second DeleteSnapshot: err=%v, want ErrSnapshotNotFound", err)
	}
}

func TestManagerSnapshotUnknownSandbox(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Snapshot(context.Background(), "sbx_0000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestManagerForkUnknownSnapshot(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Fork(context.Background(), "snap_0000000000000", 1)
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("err = %v, want ErrSnapshotNotFound", err)
	}
}

func TestManagerForkRejectsZeroCount(t *testing.T) {
	m, _ := newTestManager(t)
	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fork(context.Background(), snap.ID, 0); err == nil {
		t.Error("Fork(count=0): got nil, want error")
	}
}

func TestManagerForkRollbackOnFailure(t *testing.T) {
	// Inject a restore failure after N successful ones to verify that
	// the successful forks are torn down.
	m, r := newTestManager(t)
	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}

	r.restoreErr = errors.New("restore boom")
	_, err = m.Fork(context.Background(), snap.ID, 3)
	if err == nil {
		t.Fatal("Fork: got nil, want error")
	}

	// Only the source sandbox should remain; no fork survived.
	list := m.List()
	if len(list) != 1 || list[0].ID != source.ID {
		ids := make([]string, len(list))
		for i, s := range list {
			ids[i] = s.ID
		}
		t.Errorf("sandboxes after failed Fork: %v, want only %s", ids, source.ID)
	}
}

func TestManagerForkBoundsConcurrency(t *testing.T) {
	// With ForkConcurrency=2, a fork batch larger than the cap must never
	// run more than 2 forkOne calls at once. The stub runner records the
	// peak number of simultaneous Restore calls (widened by restoreDelay).
	m, r := newTestManager(t)
	m.cfg.ForkConcurrency = 2
	r.restoreDelay = 40 * time.Millisecond

	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}

	const count = 8
	forks, err := m.Fork(context.Background(), snap.ID, count)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(forks) != count {
		t.Fatalf("len(forks) = %d, want %d", len(forks), count)
	}

	peak := atomic.LoadInt32(&r.maxInFlight)
	if peak > int32(m.cfg.ForkConcurrency) {
		t.Errorf("peak concurrent forks = %d, exceeds cap %d", peak, m.cfg.ForkConcurrency)
	}
	// Also confirm forks genuinely ran in parallel (guards against a
	// regression that serialized the fan-out to 1).
	if peak < 2 {
		t.Errorf("peak concurrent forks = %d, want the cap (2) to be reached", peak)
	}
}

func TestManagerForkRejectsOversizedCount(t *testing.T) {
	// The Manager boundary must reject count > MaxForkCount before
	// allocating results/goroutines proportional to it, independent of the
	// HTTP layer's own guard.
	m, _ := newTestManager(t)
	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Fork(context.Background(), snap.ID, m.MaxForkCount()+1)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Fork oversized count: err = %v, want ErrInvalidConfig", err)
	}
	// No fork should have been created — only the source remains.
	if list := m.List(); len(list) != 1 {
		t.Errorf("sandbox count = %d, want 1 (no forks created)", len(list))
	}
}

func TestManagerForkFailsWithoutAgentChannel(t *testing.T) {
	// A restored fork with no vsock → nil execClient → clone-safety can't
	// be applied, so the fork must fail rather than register un-refreshed.
	m, r := newTestManager(t)
	source, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}

	r.t = nil // stop Restore serving a stub agent → forks get no channel
	_, err = m.Fork(context.Background(), snap.ID, 1)
	if err == nil || !strings.Contains(err.Error(), "no agent channel") {
		t.Fatalf("Fork without agent channel: err = %v, want 'no agent channel'", err)
	}
	// The failed fork must roll back — only the source remains.
	if list := m.List(); len(list) != 1 {
		t.Errorf("sandbox count = %d, want 1 (fork rolled back)", len(list))
	}
}

func TestManagerCreateRejectsOversizedResources(t *testing.T) {
	m, _ := newTestManager(t)
	cases := map[string]CreateConfig{
		"vcpus":  {VCPUs: MaxVCPUs + 1},
		"memory": {MemoryMiB: MaxMemoryMiB + 1},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := m.Create(context.Background(), cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Create: err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestManagerLifetimeTimeoutAutoDeletes(t *testing.T) {
	m, _ := newTestManager(t)
	// 1 second is the smallest granularity the CreateConfig exposes; use
	// it directly so we're testing the real code path. Polling gives us
	// slack for CI jitter.
	s, err := m.Create(context.Background(), CreateConfig{TimeoutSec: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := m.Get(s.ID); errors.Is(err, ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s still present after %v — lifetime timer never fired", s.ID, time.Since(s.CreatedAt))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManagerLifetimeTimerExitsOnEarlyDelete(t *testing.T) {
	// The timer goroutine should exit promptly when the sandbox is
	// deleted by another path (no leak, no double-delete attempt).
	// We can't directly observe the goroutine, but `-race` + a quick
	// user-delete is enough to catch a bug here.
	m, _ := newTestManager(t)
	s, err := m.Create(context.Background(), CreateConfig{TimeoutSec: 60})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(context.Background(), s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second delete returns ErrNotFound regardless of whether the timer
	// already fired — if we double-deleted we'd see test races under -race.
	if err := m.Delete(context.Background(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete err = %v, want ErrNotFound", err)
	}
}

func TestManagerConcurrentCreateDelete(t *testing.T) {
	// Exercise the locking. Run create+delete in parallel from many
	// goroutines; -race will catch data races.
	m, _ := newTestManager(t)
	const goroutines = 16
	const perG = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				s, err := m.Create(context.Background(), CreateConfig{})
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				if err := m.Delete(context.Background(), s.ID); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := len(m.List()); n != 0 {
		t.Errorf("after parallel create/delete, %d sandboxes remain", n)
	}
}

// --- restart / reconcile tests ----------------------------------------

// newPersistentManager builds a Manager backed by a stubRunner whose
// registry journal + workdirs live under a caller-shared workBase, so two
// successive managers can simulate a daemon restart over the same state.
func newPersistentManager(t *testing.T, workBase, statePath, template string) *Manager {
	t.Helper()
	m, err := NewManager(ManagerConfig{
		Runner:    &stubRunner{t: t},
		WorkBase:  workBase,
		Kernel:    "/fake/vmlinux",
		Rootfs:    template,
		StatePath: statePath,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestManagerReconcileAfterRestart(t *testing.T) {
	workBase := t.TempDir()
	tmplDir := t.TempDir()
	template := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake-template-bytes"), 0o640); err != nil {
		t.Fatalf("write template: %v", err)
	}
	statePath := filepath.Join(workBase, "registry.jsonl")

	// --- first daemon incarnation: two sandboxes + a snapshot ---------
	m1 := newPersistentManager(t, workBase, statePath, template)
	s1, err := m1.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	s2, err := m1.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	snap, err := m1.Snapshot(context.Background(), s1.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Everything is on disk.
	for _, d := range []string{s1.Workdir, s2.Workdir, snap.Dir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("expected %s on disk: %v", d, err)
		}
	}

	// Simulate a crash: no Shutdown/Delete. The records + dirs persist.
	// A real crash closes the fd for us; emulate that so the second
	// manager can reopen the journal for append.
	if err := m1.store.close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// --- second incarnation: reconcile --------------------------------
	m2 := newPersistentManager(t, workBase, statePath, template)
	if err := m2.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Orphaned sandboxes are reaped: gone from the registry AND disk.
	for _, s := range []*Sandbox{s1, s2} {
		if _, err := m2.Get(s.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("sandbox %s still registered after reconcile: err=%v", s.ID, err)
		}
		if _, err := os.Stat(s.Workdir); !os.IsNotExist(err) {
			t.Errorf("sandbox workdir %s not reaped: err=%v", s.Workdir, err)
		}
	}

	// The snapshot is re-adopted: registered, files intact, forkable.
	got, err := m2.GetSnapshot(snap.ID)
	if err != nil {
		t.Fatalf("snapshot not re-adopted: %v", err)
	}
	if got.RootfsPath != snap.RootfsPath || got.SourceID != snap.SourceID {
		t.Errorf("re-adopted snapshot fields drifted: got %+v want rootfs=%s source=%s", got, snap.RootfsPath, snap.SourceID)
	}
	if _, err := os.Stat(snap.Dir); err != nil {
		t.Errorf("snapshot dir removed by reconcile: %v", err)
	}
	forks, err := m2.Fork(context.Background(), snap.ID, 2)
	if err != nil {
		t.Fatalf("Fork from re-adopted snapshot: %v", err)
	}
	if len(forks) != 2 {
		t.Errorf("len(forks) = %d, want 2", len(forks))
	}

	// The journal was compacted down to the single surviving snapshot:
	// replaying it yields no sandboxes and one snapshot record. (The
	// forks above were created post-reconcile and re-appended, so read
	// the journal state that existed right after compaction by checking
	// the reaped sandboxes are absent.)
	sbxRecs, snapRecs, err := replayJournal(statePath)
	if err != nil {
		t.Fatalf("replay journal: %v", err)
	}
	for _, r := range sbxRecs {
		if r.ID == s1.ID || r.ID == s2.ID {
			t.Errorf("reaped sandbox %s still in journal after compaction", r.ID)
		}
	}
	var foundSnap bool
	for _, r := range snapRecs {
		if r.ID == snap.ID {
			foundSnap = true
		}
	}
	if !foundSnap {
		t.Errorf("adopted snapshot %s missing from compacted journal", snap.ID)
	}

	m2.Shutdown(context.Background())
}

func TestManagerReconcileSweepsUnjournaledOrphan(t *testing.T) {
	workBase := t.TempDir()
	tmplDir := t.TempDir()
	template := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake-template-bytes"), 0o640); err != nil {
		t.Fatalf("write template: %v", err)
	}
	statePath := filepath.Join(workBase, "registry.jsonl")

	// Simulate a crash mid-Create: a workdir tree exists on disk with no
	// journal record (the daemon died before the put was appended).
	orphan := filepath.Join(workBase, "sbx_orphan0000001")
	if err := os.MkdirAll(filepath.Join(orphan, "sub"), 0o750); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	m := newPersistentManager(t, workBase, statePath, template)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("un-journaled orphan dir not swept: err=%v", err)
	}
	// The journal itself must survive the sweep.
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("registry journal removed by sweep: %v", err)
	}
	m.Shutdown(context.Background())
}

func TestStoreToleratesTornFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	// One valid snapshot put, then a torn (truncated, newline-less) line
	// as a crash mid-append would leave.
	good := `{"op":"put","kind":"snapshot","id":"snap_aaaaaaaaaaaaa","snapshot":{"id":"snap_aaaaaaaaaaaaa","dir":"/x"}}` + "\n"
	torn := `{"op":"put","kind":"sandbox","id":"sbx_bbb`
	if err := os.WriteFile(path, []byte(good+torn), 0o640); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	st, sbx, snaps, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if len(sbx) != 0 {
		t.Errorf("torn sandbox line should be skipped, got %d sandbox records", len(sbx))
	}
	if len(snaps) != 1 || snaps[0].ID != "snap_aaaaaaaaaaaaa" {
		t.Errorf("good snapshot lost on torn-line replay: %v", snaps)
	}

	// A new append must not concatenate onto the torn bytes.
	if err := st.putSnapshot(snapshotRecord{ID: "snap_ccccccccccccc", Dir: "/y"}); err != nil {
		t.Fatalf("putSnapshot: %v", err)
	}
	if err := st.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, snaps2, err := replayJournal(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	got := make(map[string]bool)
	for _, s := range snaps2 {
		got[s.ID] = true
	}
	if !got["snap_aaaaaaaaaaaaa"] || !got["snap_ccccccccccccc"] {
		t.Errorf("expected both snapshots after torn-line recovery, got %v", snaps2)
	}
}

func TestManagerReconcileDropsSnapshotWithMissingFiles(t *testing.T) {
	workBase := t.TempDir()
	tmplDir := t.TempDir()
	template := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake-template-bytes"), 0o640); err != nil {
		t.Fatalf("write template: %v", err)
	}
	statePath := filepath.Join(workBase, "registry.jsonl")

	m1 := newPersistentManager(t, workBase, statePath, template)
	src, err := m1.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap, err := m1.Snapshot(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := m1.store.close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Corrupt the durable state: the snapshot's files vanished (disk
	// wiped, manual rm, etc.) while its record survived in the journal.
	if err := os.RemoveAll(snap.Dir); err != nil {
		t.Fatalf("remove snapshot dir: %v", err)
	}

	m2 := newPersistentManager(t, workBase, statePath, template)
	if err := m2.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The dangling snapshot must be dropped, not re-adopted.
	if _, err := m2.GetSnapshot(snap.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("dangling snapshot re-adopted: err=%v, want ErrSnapshotNotFound", err)
	}
	if n := len(m2.ListSnapshots()); n != 0 {
		t.Errorf("ListSnapshots = %d, want 0 after dropping dangling snapshot", n)
	}
}
