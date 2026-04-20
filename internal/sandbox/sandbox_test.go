package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

func (h *stubHandle) Workdir() string { return h.workdir }
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
	mu       sync.Mutex
	calls    []runner.Spec
	startErr error
	handles  []*stubHandle
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

// newTestManager constructs a Manager backed by a fresh stubRunner.
func newTestManager(t *testing.T) (*Manager, *stubRunner) {
	t.Helper()
	workBase := t.TempDir()
	r := &stubRunner{}
	m, err := NewManager(ManagerConfig{
		Runner:   r,
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   "/fake/rootfs.ext4",
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

	// Verify the runner received the spec we computed.
	if len(r.calls) != 1 {
		t.Fatalf("runner.Start called %d times, want 1", len(r.calls))
	}
	got := r.calls[0]
	if got.Kernel != "/fake/vmlinux" || got.Rootfs != "/fake/rootfs.ext4" {
		t.Errorf("spec paths wrong: %+v", got)
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

func TestManagerCreateRunnerErrorLeavesMapClean(t *testing.T) {
	wantErr := errors.New("start boom")
	workBase := t.TempDir()
	r := &stubRunner{startErr: wantErr}
	m, err := NewManager(ManagerConfig{
		Runner:   r,
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   "/fake/rootfs.ext4",
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
