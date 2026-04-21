package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	shutdown chan struct{}
	shutErr  error
}

func newStubHandle(workdir string) *stubHandle {
	return &stubHandle{workdir: workdir, shutdown: make(chan struct{})}
}

func (h *stubHandle) Workdir() string                               { return h.workdir }
func (h *stubHandle) VSockPath() string                             { return "" } // stub handle has no vsock
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
	mu           sync.Mutex
	calls        []runner.Spec
	restoreCalls []runner.RestoreSpec
	startErr     error
	restoreErr   error
	handles      []*stubHandle
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restoreCalls = append(r.restoreCalls, spec)
	if r.restoreErr != nil {
		return nil, r.restoreErr
	}
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
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

	r := &stubRunner{}
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

	// Each fork should have its own workdir + its own memory and rootfs files.
	for _, f := range forks {
		if !strings.HasPrefix(f.ID, "sbx_") {
			t.Errorf("fork id %q: missing sbx_ prefix", f.ID)
		}
		if f.VCPUs != snap.VCPUs || f.MemoryMiB != snap.MemoryMiB {
			t.Errorf("fork sizing = %d/%d, want %d/%d from snapshot",
				f.VCPUs, f.MemoryMiB, snap.VCPUs, snap.MemoryMiB)
		}
		for _, name := range []string{perSandboxRootfsName, perForkMemoryName} {
			if _, err := os.Stat(filepath.Join(f.Workdir, name)); err != nil {
				t.Errorf("fork %s missing %s: %v", f.ID, name, err)
			}
		}
	}

	// The runner should have seen three Restore calls.
	if len(r.restoreCalls) != 3 {
		t.Errorf("runner.Restore called %d times, want 3", len(r.restoreCalls))
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
