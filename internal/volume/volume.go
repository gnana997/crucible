// Package volume manages persistent block-device volumes: sparse backing
// files under a daemon-configured directory, formatted ext4 on first use and
// attached to a sandbox as a Firecracker drive. A volume outlives the
// sandbox it attaches to; an in-memory single-writer guard prevents two live
// sandboxes from mounting the same volume (ext4 corrupts under two writers).
//
// V-M1 scope: provision + attach-guard, sandbox-only. The durable volumes
// store, explicit sizing (`volume create`), and lifecycle CLI/API arrive in
// V-M2. Fast snapshot-wake with a volume is F3-full (v0.6.2).
package volume

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
)

// DefaultSize is the size a volume's backing file is created at when no
// explicit size is given. V-M1 has no `volume create`, so every volume uses
// this; explicit per-volume sizing lands in V-M2.
const DefaultSize = 2 << 30 // 2 GiB

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var (
	// ErrInUse means the volume is already attached to another live sandbox.
	ErrInUse = errors.New("volume: already attached to another sandbox")
	// ErrInvalidName means the name isn't a safe filename token.
	ErrInvalidName = errors.New("volume: name must match [a-z0-9][a-z0-9-]* (max 63 chars)")
)

// Manager provisions and tracks volumes. Safe for concurrent use.
type Manager struct {
	dir         string
	defaultSize int64
	uid, gid    int

	mu       sync.Mutex
	attached map[string]string // volume name -> sandbox id holding the single-writer claim
}

// NewManager creates the volume directory and preflights mkfs.ext4. uid/gid
// are the jailer's user/group so backing files are chowned to the user
// firecracker runs as (which must be able to open them read-write); pass the
// daemon's own uid/gid for the direct-exec (non-jailer) runner. defaultSize
// <= 0 falls back to DefaultSize.
func NewManager(dir string, defaultSize int64, uid, gid int) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("volume: dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("volume: create dir %s: %w", dir, err)
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return nil, fmt.Errorf("volume: mkfs.ext4 not found on PATH (install e2fsprogs): %w", err)
	}
	if defaultSize <= 0 {
		defaultSize = DefaultSize
	}
	return &Manager{
		dir:         dir,
		defaultSize: defaultSize,
		uid:         uid,
		gid:         gid,
		attached:    make(map[string]string),
	}, nil
}

// Attach claims the named volume for sandboxID and ensures its backing file
// exists (created + formatted ext4 on first use). Returns the absolute host
// path of the backing file (to hand the runner as a drive). Fails with
// ErrInUse if another live sandbox holds it (ext4 is single-writer).
func (m *Manager) Attach(name, sandboxID string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.attached[name]; ok && holder != sandboxID {
		return "", fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	path := filepath.Join(m.dir, name+".img")
	if err := m.provision(path); err != nil {
		return "", err
	}
	m.attached[name] = sandboxID
	return path, nil
}

// Release drops the in-memory attach claim for name (idempotent). The backing
// file is left in place — volumes are durable and survive their sandbox.
func (m *Manager) Release(name string) {
	m.mu.Lock()
	delete(m.attached, name)
	m.mu.Unlock()
}

// provision creates + formats the backing file on first use. The format runs
// against a temp file that is renamed to the final path only after mkfs
// succeeds, so a crash mid-format never leaves a half-formatted file that the
// "exists ⇒ formatted" check would wrongly trust. Idempotent: an existing
// backing file is left untouched (that is how data persists across sandboxes).
// Caller holds m.mu.
func (m *Manager) provision(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // exists ⇒ already provisioned + formatted
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: stat %s: %w", path, err)
	}

	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("volume: create %s: %w", tmp, err)
	}
	if err := f.Truncate(m.defaultSize); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: size %s: %w", tmp, err)
	}
	_ = f.Close()

	// -F force on a plain file; -q quiet; -m 0 no reserved-for-root blocks
	// (this is a data disk, not a root filesystem).
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", "-m", "0", tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: mkfs.ext4 %s: %w: %s", tmp, err, string(out))
	}
	if err := os.Chown(tmp, m.uid, m.gid); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: chown %s to %d:%d: %w", tmp, m.uid, m.gid, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: finalize %s: %w", path, err)
	}
	return nil
}
