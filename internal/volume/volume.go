// Package volume manages persistent block-device volumes: sparse backing
// files under a daemon-configured directory, formatted ext4 on first use and
// attached to a sandbox as a Firecracker drive. A volume outlives the sandbox
// it attaches to; an in-memory single-writer guard prevents two live sandboxes
// from mounting the same volume (ext4 corrupts under two writers).
//
// v0.6.0 shipped attach/mount/format + the guard, plus a durable bbolt
// record store (survives restart), explicit lifecycle (Create/List/Remove),
// and a host-pin. Fast snapshot-wake with a volume shipped in v0.6.2.
package volume

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/fsutil"
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
	// ErrBackupNotFound means no backup of that id exists.
	ErrBackupNotFound = errors.New("volume: backup not found")
)

// Info is a volume record annotated with its live attachment.
type Info struct {
	Record
	AttachedTo string `json:"attached_to,omitempty"` // sandbox id, "" if detached
}

// Manager provisions and tracks volumes. Safe for concurrent use.
type Manager struct {
	dir           string
	backupDir     string // where volume backups are written (default <dir>/backups)
	backupReflink bool   // a backup Clone into backupDir is O(1) reflink (not a byte copy)
	defaultSize   int64
	hostID        string
	uid, gid      int
	st            *store

	mu       sync.Mutex
	attached map[string]string // volume name -> sandbox id holding the single-writer claim
}

// NewManager opens (creating if absent) the volume directory + record store,
// preflights mkfs.ext4, and back-fills records for any pre-existing backing
// files (volumes created before the record store). uid/gid are the user firecracker runs as (jailer
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
		backupDir:   filepath.Join(dir, "backups"),
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
	m.probeBackupReflink()
	return m, nil
}

// probeBackupReflink records whether a backup Clone (volume dir → backup dir)
// would be an O(1) reflink or a full byte copy, gating no-downtime live backups
// (worth freezing the guest only for the O(1) case). Called at startup and when
// the backup dir changes.
func (m *Manager) probeBackupReflink() {
	_ = os.MkdirAll(m.backupDir, 0o700)
	m.backupReflink = fsutil.CanReflink(m.dir, m.backupDir)
}

// BackupReflinks reports whether backups into the configured backup dir use an
// O(1) reflink. Live (fsfreeze) backups are only allowed when true.
func (m *Manager) BackupReflinks() bool { return m.backupReflink }

// Close releases the store's file lock.
func (m *Manager) Close() error { return m.st.close() }

// backfill inserts a record for any *.img backing file that has none — so
// volumes created before the store existed still appear in List.
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

// BackupStoreTo streams a consistent copy of the volume-record store's bbolt
// file (records + backup catalog, NOT volume data — data is `volume backup`'s
// job). Used by the daemon's daemon backup; see store.backupTo.
func (m *Manager) BackupStoreTo(frame func(size int64) (io.Writer, error)) error {
	return m.st.backupTo(frame)
}

// DiskBytes returns the allocated on-disk bytes of all volume backing files
// (sparse-aware: a mostly-empty volume counts what it occupies, not its
// provisioned size). Gauge source, read at scrape time; errors count as 0 — a
// metrics read must never fail.
func (m *Manager) DiskBytes() int64 {
	recs, err := m.st.list()
	if err != nil {
		return 0
	}
	var total int64
	for _, r := range recs {
		total += fsutil.AllocatedBytes(filepath.Join(m.dir, r.Name+".img"))
	}
	return total
}

// BackupDiskBytes returns the allocated on-disk bytes of all volume backups.
// Reflink-shared blocks (a same-filesystem backup) are counted per file, so
// this reports logical allocation, not unique physical blocks. Gauge source,
// same error contract as DiskBytes.
func (m *Manager) BackupDiskBytes() int64 {
	recs, err := m.st.listBackups()
	if err != nil {
		return 0
	}
	var total int64
	for _, r := range recs {
		total += fsutil.AllocatedBytes(r.Path)
	}
	return total
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

// SetBackupDir overrides where backups are written (default <volume-dir>/backups).
// Call once at startup, before serving requests. An empty dir keeps the default.
func (m *Manager) SetBackupDir(dir string) {
	if dir != "" {
		m.backupDir = dir
		m.probeBackupReflink()
	}
}

// Backup takes a point-in-time copy of a volume's backing file into the backup
// dir and records it, returning the backup metadata. The copy is O(1) via reflink
// when the backup dir shares the volume dir's filesystem, else a byte-copy.
//
// The result is filesystem-consistent only if the volume is quiescent — detached
// (no writer) or slept (VMM stopped, backing file already host-fsync'd). Backup
// itself does NOT verify that: the caller (the daemon handler) classifies the
// volume's holder run-state and refuses a live backup (live/frozen backup lands
// with the fsfreeze agent op in a later milestone). ErrNotFound if the volume
// doesn't exist.
func (m *Manager) Backup(name string) (BackupRecord, error) {
	if !nameRe.MatchString(name) {
		return BackupRecord{}, ErrInvalidName
	}
	rec, ok, err := m.st.get(name)
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok {
		return BackupRecord{}, ErrNotFound
	}
	// Flush any writeback still buffered host-side so the copy is durable.
	if err := m.Sync(name); err != nil {
		return BackupRecord{}, err
	}
	id := name + "-" + time.Now().UTC().Format("20060102T150405.000Z")
	destDir := filepath.Join(m.backupDir, name)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return BackupRecord{}, fmt.Errorf("volume: create backup dir %s: %w", destDir, err)
	}
	dst := filepath.Join(destDir, id+".img")
	if err := fsutil.Clone(filepath.Join(m.dir, name+".img"), dst); err != nil {
		return BackupRecord{}, fmt.Errorf("volume: backup %s: %w", name, err)
	}
	brec := BackupRecord{
		ID: id, SourceVolume: name, SizeBytes: rec.SizeBytes,
		CreatedAt: time.Now().UTC(), Consistency: "filesystem", HostID: m.hostID, Path: dst,
	}
	if err := m.st.putBackup(brec); err != nil {
		_ = os.Remove(dst)
		return BackupRecord{}, err
	}
	return brec, nil
}

// ListBackups returns backups (newest first), filtered to one source volume when
// sourceVol is non-empty (all backups when empty).
func (m *Manager) ListBackups(sourceVol string) ([]BackupRecord, error) {
	recs, err := m.st.listBackups()
	if err != nil {
		return nil, err
	}
	out := recs[:0]
	for _, r := range recs {
		if sourceVol == "" || r.SourceVolume == sourceVol {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// GetBackup returns one backup by id. ErrBackupNotFound if absent.
func (m *Manager) GetBackup(id string) (BackupRecord, error) {
	rec, ok, err := m.st.getBackup(id)
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok {
		return BackupRecord{}, ErrBackupNotFound
	}
	return rec, nil
}

// OpenBackup opens a backup's backing file for reading (streaming it off-host),
// returning the open file, its record, and the on-disk byte size. The caller
// closes the file. ErrBackupNotFound if the record or file is gone. The file is
// a static, already-consistent point-in-time image, so no quiesce is needed.
func (m *Manager) OpenBackup(id string) (*os.File, BackupRecord, int64, error) {
	rec, ok, err := m.st.getBackup(id)
	if err != nil {
		return nil, BackupRecord{}, 0, err
	}
	if !ok {
		return nil, BackupRecord{}, 0, ErrBackupNotFound
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, BackupRecord{}, 0, ErrBackupNotFound
		}
		return nil, BackupRecord{}, 0, fmt.Errorf("volume: open backup %s: %w", id, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, BackupRecord{}, 0, fmt.Errorf("volume: stat backup %s: %w", id, err)
	}
	return f, rec, fi.Size(), nil
}

// DeleteBackup removes a backup's backing file and record. ErrBackupNotFound if
// absent.
func (m *Manager) DeleteBackup(id string) error {
	rec, ok, err := m.st.getBackup(id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBackupNotFound
	}
	if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: remove backup file: %w", err)
	}
	return m.st.delBackup(id)
}

// RestoreTo materialises a backup into a NEW volume named newName, returning its
// record. Refuses to overwrite an existing volume (ErrExists) — restore never
// clobbers live data; use a fresh name. ErrBackupNotFound if the backup is gone.
// The restored image mounts read-write in a guest and replays its journal.
func (m *Manager) RestoreTo(backupID, newName string) (Record, error) {
	if !nameRe.MatchString(newName) {
		return Record{}, ErrInvalidName
	}
	brec, ok, err := m.st.getBackup(backupID)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrBackupNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok, err := m.st.get(newName); err != nil {
		return Record{}, err
	} else if ok {
		return Record{}, fmt.Errorf("%w: %s", ErrExists, newName)
	}
	return m.materialize(newName, brec.Path, brec.SizeBytes)
}

// Clone copies a quiescent source volume into a NEW volume dst, returning dst's
// record. Refuses to overwrite an existing volume (ErrExists); ErrNotFound if src
// doesn't exist. Like Backup, Clone does NOT verify src is quiescent — the daemon
// handler refuses a live source (it copies the raw backing file).
func (m *Manager) Clone(src, dst string) (Record, error) {
	if !nameRe.MatchString(src) || !nameRe.MatchString(dst) {
		return Record{}, ErrInvalidName
	}
	srcRec, ok, err := m.st.get(src)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrNotFound
	}
	// Flush host-buffered writeback so the copy is durable (safe: caller vouched
	// the source is quiescent). Sync takes m.mu, so call it before locking.
	if err := m.Sync(src); err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok, err := m.st.get(dst); err != nil {
		return Record{}, err
	} else if ok {
		return Record{}, fmt.Errorf("%w: %s", ErrExists, dst)
	}
	return m.materialize(dst, filepath.Join(m.dir, src+".img"), srcRec.SizeBytes)
}

// materialize clones srcPath into a new volume named name (its backing file +
// record), chowning the copy so a jailed firecracker can open it. Caller holds
// m.mu and has verified name is free.
func (m *Manager) materialize(name, srcPath string, sizeBytes int64) (Record, error) {
	dstPath := filepath.Join(m.dir, name+".img")
	if err := fsutil.Clone(srcPath, dstPath); err != nil {
		return Record{}, fmt.Errorf("volume: materialize %s: %w", name, err)
	}
	if err := os.Chown(dstPath, m.uid, m.gid); err != nil {
		_ = os.Remove(dstPath)
		return Record{}, fmt.Errorf("volume: chown %s: %w", dstPath, err)
	}
	rec := Record{Name: name, SizeBytes: sizeBytes, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}
	if err := m.st.put(rec); err != nil {
		_ = os.Remove(dstPath)
		return Record{}, err
	}
	return rec, nil
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
