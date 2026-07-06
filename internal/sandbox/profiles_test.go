package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestManagerWithProfiles builds a Manager with a default rootfs plus a
// "python-3.12" profile. The template files carry distinct content so a
// test can prove which one was cloned.
func newTestManagerWithProfiles(t *testing.T) *Manager {
	t.Helper()
	tmplDir := t.TempDir()

	def := filepath.Join(tmplDir, "rootfs.ext4")
	if err := os.WriteFile(def, []byte("default-image"), 0o640); err != nil {
		t.Fatalf("write default rootfs: %v", err)
	}
	py := filepath.Join(tmplDir, "python-3.12.ext4")
	if err := os.WriteFile(py, []byte("python-image"), 0o640); err != nil {
		t.Fatalf("write python rootfs: %v", err)
	}

	m, err := NewManager(ManagerConfig{
		Runner:   &stubRunner{t: t},
		WorkBase: t.TempDir(),
		Kernel:   "/fake/vmlinux",
		Rootfs:   def,
		Profiles: map[string]string{"python-3.12": py},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func cloneContent(t *testing.T, s *Sandbox) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Workdir, perSandboxRootfsName))
	if err != nil {
		t.Fatalf("read per-sandbox rootfs: %v", err)
	}
	return string(b)
}

func TestCreateWithProfileClonesProfileImage(t *testing.T) {
	m := newTestManagerWithProfiles(t)
	s, err := m.Create(context.Background(), CreateConfig{Profile: "python-3.12"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Profile != "python-3.12" {
		t.Errorf("Sandbox.Profile = %q, want python-3.12", s.Profile)
	}
	if got := cloneContent(t, s); got != "python-image" {
		t.Errorf("cloned wrong template: content = %q, want python-image", got)
	}
}

func TestCreateNoProfileUsesDefaultRootfs(t *testing.T) {
	m := newTestManagerWithProfiles(t)
	s, err := m.Create(context.Background(), CreateConfig{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Profile != "" {
		t.Errorf("Sandbox.Profile = %q, want empty", s.Profile)
	}
	if got := cloneContent(t, s); got != "default-image" {
		t.Errorf("default did not clone default rootfs: content = %q", got)
	}
}

func TestCreateUnknownProfileRejectedNoSideEffects(t *testing.T) {
	m := newTestManagerWithProfiles(t)
	_, err := m.Create(context.Background(), CreateConfig{Profile: "ruby"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Create with unknown profile: err = %v, want ErrInvalidConfig", err)
	}
	// The request must fail before any workdir is created.
	entries, err := os.ReadDir(m.cfg.WorkBase)
	if err != nil {
		t.Fatalf("read workbase: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("unknown profile left %d workdir(s) behind", len(entries))
	}
}
