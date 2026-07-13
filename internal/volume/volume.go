// Package volume manages persistent block-device volumes: sparse backing
// files under a daemon-configured directory, formatted ext4 on first use and
// attached to a sandbox as a Firecracker drive. A volume outlives the sandbox
// it attaches to; an in-memory single-writer guard prevents two live sandboxes
// from mounting the same volume (ext4 corrupts under two writers).
//
// V-M1 shipped attach/mount/format + the guard. V-M2 adds a durable bbolt
// record store (survives restart), explicit lifecycle (Create/List/Remove),
// and a host-pin. Fast snapshot-wake with a volume is F3-full (v0.6.2).
package volume

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultSize is the size a volume's backing file is created at when no
// explicit size is given (`run --volume name:/path` with no prior
// `volume create`).
const DefaultSize = 2 << 30 // 2 GiB

// storeFile is the bbolt index kept alongside the backing files.
const storeFile = "index.db"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var (
	// ErrInUse means the volume is attached to a live sandbox.
	ErrInUse = errors.New("volume: in use by a live sandbox")
	// ErrInvalidName means the name isn't a safe filename token.
	ErrInvalidName = errors.New("volume: name must match [a-z0-9][a-z0-9-]* (max 63 chars)")
	// ErrExists means a volume of that name already exists.
	ErrExists = errors.New("volume: already exists")
	// ErrNotFound means no volume of that name exists.
	ErrNotFound = errors.New("volume: not found")
)

// Info is a volume record annotated with its live attachment.
type Info struct {
	Record
	AttachedTo string `json:"attached_to,omitempty"` // sandbox id, "" if detached
}

// Manager provisions and tracks volumes. Safe for concurrent use.
type Manager struct {
	dir         string
	defaultSize int64
	hostID      string
	uid, gid    int
	st          *store

	mu       sync.Mutex
	attached map[string]string // volume name -> sandbox id holding the single-writer claim
}

// NewManager opens (creating if absent) the volume directory + record store,
// preflights mkfs.ext4, and back-fills records for any pre-existing backing
// files (V-M1 volumes). uid/gid are the user firecracker runs as (jailer
// uid/gid under jailer; the daemon's own for direct-exec) so backing files are
// openable. hostID is the daemon's host identity (host-pin). defaultSize <= 0
// falls back to DefaultSize.
func NewManager(dir string, defaultSize int64, hostID string, uid, gid int) (*Manager, error) {
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
	st, err := openStore(filepath.Join(dir, storeFile))
	if err != nil {
		return nil, err
	}
	m := &Manager{
		dir:         dir,
		defaultSize: defaultSize,
		hostID:      hostID,
		uid:         uid,
		gid:         gid,
		st:          st,
		attached:    make(map[string]string),
	}
	if err := m.backfill(); err != nil {
		_ = st.close()
		return nil, err
	}
	return m, nil
}

// Close releases the store's file lock.
func (m *Manager) Close() error { return m.st.close() }

// backfill inserts a record for any *.img backing file that has none — so
// volumes created before the store existed (V-M1) still appear in List.
func (m *Manager) backfill() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("volume: scan %s: %w", m.dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".img") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".img")
		if !nameRe.MatchString(name) {
			continue
		}
		if _, ok, err := m.st.get(name); err != nil {
			return err
		} else if ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		_ = m.st.put(Record{
			Name: name, SizeBytes: fi.Size(), CreatedAt: fi.ModTime().UTC(),
			Formatted: true, HostID: m.hostID,
		})
	}
	return nil
}

// Create explicitly provisions a new volume at sizeBytes (<=0 → default),
// recording it durably. Errors with ErrExists if the name is taken.
func (m *Manager) Create(name string, sizeBytes int64) (Record, error) {
	if !nameRe.MatchString(name) {
		return Record{}, ErrInvalidName
	}
	if sizeBytes <= 0 {
		sizeBytes = m.defaultSize
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok, err := m.st.get(name); err != nil {
		return Record{}, err
	} else if ok {
		return Record{}, fmt.Errorf("%w: %s", ErrExists, name)
	}
	path := filepath.Join(m.dir, name+".img")
	if err := m.provision(path, sizeBytes); err != nil {
		return Record{}, err
	}
	rec := Record{Name: name, SizeBytes: sizeBytes, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}
	if err := m.st.put(rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// Attach claims the named volume for sandboxID and ensures its backing file
// exists (created + formatted on first use, at the recorded size — or the
// default size, auto-creating a record, when the volume is new). Returns the
// absolute host path. ErrInUse if another live sandbox holds it.
func (m *Manager) Attach(name, sandboxID string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.attached[name]; ok && holder != sandboxID {
		return "", fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	rec, known, err := m.st.get(name)
	if err != nil {
		return "", err
	}
	size := m.defaultSize
	if known {
		size = rec.SizeBytes
	}
	path := filepath.Join(m.dir, name+".img")
	if err := m.provision(path, size); err != nil {
		return "", err
	}
	if !known {
		if err := m.st.put(Record{Name: name, SizeBytes: size, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}); err != nil {
			return "", err
		}
	}
	m.attached[name] = sandboxID
	return path, nil
}

// Release drops the in-memory attach claim for name (idempotent). The backing
// file + record are left in place — volumes are durable.
func (m *Manager) Release(name string) {
	m.mu.Lock()
	delete(m.attached, name)
	m.mu.Unlock()
}

// List returns every volume record annotated with its live attachment.
func (m *Manager) List() ([]Info, error) {
	recs, err := m.st.list()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(recs))
	for _, r := range recs {
		out = append(out, Info{Record: r, AttachedTo: m.attached[r.Name]})
	}
	return out, nil
}

// Get returns one volume's info. ErrNotFound if it doesn't exist.
func (m *Manager) Get(name string) (Info, error) {
	rec, ok, err := m.st.get(name)
	if err != nil {
		return Info{}, err
	}
	if !ok {
		return Info{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Info{Record: rec, AttachedTo: m.attached[name]}, nil
}

// Remove deletes the volume's record and backing file. ErrInUse if a live
// sandbox holds it; ErrNotFound if it doesn't exist.
func (m *Manager) Remove(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.attached[name]; ok {
		return fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	if _, ok, err := m.st.get(name); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	if err := m.st.del(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.dir, name+".img")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: remove backing file: %w", err)
	}
	return nil
}

// Sync fsyncs a volume's backing file so writes the host buffered for it
// (cache_type=Writeback defers them until a guest FLUSH) reach persistent
// storage. Called at snapshot-sleep time: Firecracker does NOT flush drive
// backing files when it takes a snapshot, so without this an app that is slept
// (snapshot + VMM stopped) could lose committed rows if the host crashes while
// it is asleep — breaking the fsync-honest durability guarantee. Idempotent and
// cheap (a no-op when nothing is dirty). ErrInvalidName / ErrNotFound on a bad
// or unknown name. The caller must have quiesced the guest and paused the VM
// first, so no new writes are in flight.
func (m *Manager) Sync(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	f, err := os.OpenFile(filepath.Join(m.dir, name+".img"), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("volume: open %s for sync: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("volume: fsync %s: %w", name, err)
	}
	return nil
}

// provision creates + formats the backing file at sizeBytes on first use. mkfs
// runs against a temp file renamed only after it succeeds, so a crash
// mid-format never leaves a half-formatted file the "exists ⇒ formatted" check
// would trust. Idempotent: an existing backing file is left untouched. Caller
// holds m.mu.
func (m *Manager) provision(path string, sizeBytes int64) error {
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
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: size %s: %w", tmp, err)
	}
	_ = f.Close()

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
