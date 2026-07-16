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
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/cryptdev"
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
	// ErrEncryptionDisabled means an encrypted volume was requested but no master
	// key is configured (EnableEncryption was never called).
	ErrEncryptionDisabled = errors.New("volume: encryption not enabled (no master key configured)")
	// ErrNotEncrypted means an encryption-only operation (Shred, OpenDevice) was
	// called on a plaintext volume.
	ErrNotEncrypted = errors.New("volume: not an encrypted volume")
	// ErrEncryptedCloneUnsupported means Clone was asked to copy an encrypted
	// volume, which requires a fresh key (a full re-encrypt) not yet implemented.
	ErrEncryptedCloneUnsupported = errors.New("volume: cloning an encrypted volume is not yet supported (restore a backup instead)")
	// ErrKeyNotFound means a volume's (or a requested) encryption key id is not in
	// the configured keyring — e.g. the key was retired while a volume still used it.
	ErrKeyNotFound = errors.New("volume: encryption key id not found in the keyring")
	// ErrNotLarger means a grow was asked to shrink or keep the current size.
	// Volumes grow only — ext4 cannot shrink online and offline shrink is unsafe.
	ErrNotLarger = errors.New("volume: new size must be larger than the current size (grow-only, no shrink)")
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

	// Encryption (per-volume LUKS). crypt is nil unless EnableEncryption was
	// called. keys is the keyring — {keyID → 32-byte KEK}; each volume record's
	// KeyID selects which KEK wraps its per-volume DEK. defaultKeyID is the key
	// new volumes are wrapped under unless a CreateOpts overrides it.
	crypt          *cryptdev.Engine
	keysMu         sync.RWMutex // guards keys (ReloadKeyring can swap it live)
	keys           map[string][]byte
	defaultKeyID   string // set once at EnableEncryption; reload does not change it
	defaultEncrypt bool
	audit          *slog.Logger // key-operation audit trail; nil = off

	mu       sync.Mutex
	attached map[string]string // volume name -> sandbox id holding the single-writer claim
}

// EnableEncryption turns on per-volume LUKS encryption with a keyring of one or
// more master keys (each 32 bytes), keyed by id. defaultKeyID (which must be in
// the keyring) is the key new volumes are wrapped under, and defaultEncrypt makes
// new volumes encrypted unless a CreateOpts overrides. Call once at startup,
// before serving requests. Errors if cryptsetup is missing, a key is the wrong
// size, or defaultKeyID is absent — encryption fails loud, never silently off.
func (m *Manager) EnableEncryption(keyring map[string][]byte, defaultKeyID string, defaultEncrypt bool) error {
	if len(keyring) == 0 {
		return errors.New("volume: keyring is empty")
	}
	if _, ok := keyring[defaultKeyID]; !ok {
		return fmt.Errorf("volume: default key %q is not in the keyring", defaultKeyID)
	}
	keys := make(map[string][]byte, len(keyring))
	for id, kek := range keyring {
		if len(kek) != cryptdev.KEKSize {
			return fmt.Errorf("volume: key %q must be %d bytes, got %d", id, cryptdev.KEKSize, len(kek))
		}
		keys[id] = append([]byte(nil), kek...)
	}
	if err := cryptdev.Available(); err != nil {
		return err
	}
	m.crypt = cryptdev.New()
	m.keysMu.Lock()
	m.keys = keys
	m.keysMu.Unlock()
	m.defaultKeyID = defaultKeyID
	m.defaultEncrypt = defaultEncrypt
	// A crashed daemon can leave decrypted mapper nodes open with no live sandbox
	// holding them. Nothing is attached yet at startup, so close any of ours now —
	// a leaked open device would block a fresh attach of the same volume.
	m.reapOrphanDevices()
	return nil
}

// kekFor returns the keyring KEK for keyID (safe for concurrent use with reload).
func (m *Manager) kekFor(keyID string) ([]byte, bool) {
	m.keysMu.RLock()
	kek, ok := m.keys[keyID]
	m.keysMu.RUnlock()
	return kek, ok
}

// Rewrap re-wraps an encrypted volume's per-volume key from its current keyring
// key to toKeyID — a KEK rotation that changes ONLY the record's wrapped key, not
// the LUKS container or the data, so it is safe on a live volume. No-op if already
// wrapped under toKeyID. ErrNotFound if absent; ErrNotEncrypted for a plaintext
// volume; ErrKeyNotFound if the current or target key id is not in the keyring.
func (m *Manager) Rewrap(name, toKeyID string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	if m.crypt == nil {
		return ErrEncryptionDisabled
	}
	toKEK, ok := m.kekFor(toKeyID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrKeyNotFound, toKeyID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok, err := m.st.get(name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if !rec.Encrypted {
		return ErrNotEncrypted
	}
	if rec.KeyID == toKeyID {
		return nil
	}
	fromKeyID := rec.KeyID
	dek, err := m.dekFor(rec) // unwraps with the record's current key
	if err != nil {
		return err
	}
	wrapped, err := cryptdev.WrapKey(toKEK, dek, []byte(name))
	if err != nil {
		return err
	}
	rec.WrappedKey, rec.KeyID = wrapped, toKeyID
	if err := m.st.put(rec); err != nil {
		return err
	}
	m.auditKey("volume_key_rotated", "volume", name, "from_key_id", fromKeyID, "to_key_id", toKeyID)
	return nil
}

// RewrapAll re-wraps every encrypted volume currently wrapped under fromKeyID to
// toKeyID, returning how many were rewrapped — the "rotate every volume off an
// old key so it can be retired" operation. On a per-volume error it returns the
// count done so far plus the error.
func (m *Manager) RewrapAll(fromKeyID, toKeyID string) (int, error) {
	if m.crypt == nil {
		return 0, ErrEncryptionDisabled
	}
	if _, ok := m.kekFor(toKeyID); !ok {
		return 0, fmt.Errorf("%w: %q", ErrKeyNotFound, toKeyID)
	}
	recs, err := m.st.list()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if r.Encrypted && r.KeyID == fromKeyID {
			if err := m.Rewrap(r.Name, toKeyID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// ReloadKeyring swaps the keyring for a freshly-loaded one (e.g. after new key
// files are added), without a daemon restart. It refuses to drop a key that any
// volume still references — rewrap those volumes first. The default key id is
// unchanged (that is a startup flag).
func (m *Manager) ReloadKeyring(keyring map[string][]byte) error {
	if m.crypt == nil {
		return ErrEncryptionDisabled
	}
	if len(keyring) == 0 {
		return errors.New("volume: keyring is empty")
	}
	if _, ok := keyring[m.defaultKeyID]; !ok {
		return fmt.Errorf("volume: default key %q missing from the reloaded keyring", m.defaultKeyID)
	}
	keys := make(map[string][]byte, len(keyring))
	for id, kek := range keyring {
		if len(kek) != cryptdev.KEKSize {
			return fmt.Errorf("volume: key %q must be %d bytes, got %d", id, cryptdev.KEKSize, len(kek))
		}
		keys[id] = append([]byte(nil), kek...)
	}
	recs, err := m.st.list()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Encrypted {
			if _, ok := keys[r.KeyID]; !ok {
				return fmt.Errorf("%w: %q is still used by volume %s (rewrap it before removing the key)", ErrKeyNotFound, r.KeyID, r.Name)
			}
		}
	}
	m.keysMu.Lock()
	m.keys = keys
	m.keysMu.Unlock()
	return nil
}

// reapOrphanDevices closes any crucible-owned mapper devices found open. Called
// at startup (m.attached is empty then), so every match is an orphan from a
// previous daemon. Best-effort: a device genuinely still in use by nothing will
// close; anything unexpected is left for the next sweep. Enumerating /dev/mapper
// avoids depending on dmsetup.
func (m *Manager) reapOrphanDevices() {
	entries, err := os.ReadDir(cryptdev.MapperDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "crucible-vol-") && !strings.HasPrefix(n, "crucible-fmt-") {
			continue
		}
		m.mu.Lock()
		_, live := m.attached[strings.TrimPrefix(n, "crucible-vol-")]
		m.mu.Unlock()
		if live {
			continue
		}
		_ = m.crypt.Close(context.Background(), n)
	}
}

// EncryptionEnabled reports whether per-volume encryption is configured.
func (m *Manager) EncryptionEnabled() bool { return m.crypt != nil }

// SetAuditLogger sets the logger that records key operations (create / rotate /
// shred). Records carry volume names and key ids ONLY — never key material.
func (m *Manager) SetAuditLogger(l *slog.Logger) { m.audit = l }

// auditKey emits a key-operation audit record. attrs must never include key bytes.
func (m *Manager) auditKey(event string, attrs ...any) {
	if m.audit != nil {
		m.audit.Info(event, attrs...)
	}
}

// CreateOpts carries per-call overrides for Create. A nil Encrypt uses the
// manager's default (--volume-encrypt); a non-nil Encrypt forces the choice. An
// empty KeyID wraps the volume under the manager's default key; a non-empty KeyID
// selects another key from the keyring.
type CreateOpts struct {
	Encrypt *bool
	KeyID   string
}

// mapperName is the device-mapper name for a live volume's decrypted node.
func mapperName(vol string) string { return "crucible-vol-" + vol }

// fmtMapperName is a distinct mapper name used only while formatting a new
// volume, so a format can never collide with a live attach of the same name.
func fmtMapperName(vol string) string { return "crucible-fmt-" + vol }

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
		// A LUKS container with no record is unrecoverable (its only key lived in
		// the lost record) — never back-fill it as a plaintext volume, which would
		// mis-mount ciphertext. Detect it by its header magic (engine-independent:
		// backfill runs before EnableEncryption).
		if looksLikeLUKS(filepath.Join(m.dir, e.Name())) {
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
// recording it durably. opts.Encrypt (nil = manager default) makes it a
// per-volume LUKS container. Errors with ErrExists if the name is taken, or
// ErrEncryptionDisabled if encryption is requested but no master key is set.
func (m *Manager) Create(name string, sizeBytes int64, opts CreateOpts) (Record, error) {
	if !nameRe.MatchString(name) {
		return Record{}, ErrInvalidName
	}
	if sizeBytes <= 0 {
		sizeBytes = m.defaultSize
	}
	encrypt := m.defaultEncrypt
	if opts.Encrypt != nil {
		encrypt = *opts.Encrypt
	}
	if encrypt && m.crypt == nil {
		return Record{}, ErrEncryptionDisabled
	}
	wrapKeyID := m.defaultKeyID
	if opts.KeyID != "" {
		wrapKeyID = opts.KeyID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok, err := m.st.get(name); err != nil {
		return Record{}, err
	} else if ok {
		return Record{}, fmt.Errorf("%w: %s", ErrExists, name)
	}
	path := filepath.Join(m.dir, name+".img")
	rec := Record{Name: name, SizeBytes: sizeBytes, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}
	if encrypt {
		kek, ok := m.kekFor(wrapKeyID)
		if !ok {
			return Record{}, fmt.Errorf("%w: %q", ErrKeyNotFound, wrapKeyID)
		}
		dek, err := cryptdev.NewDEK()
		if err != nil {
			return Record{}, err
		}
		wrapped, err := cryptdev.WrapKey(kek, dek, []byte(name))
		if err != nil {
			return Record{}, err
		}
		if err := m.provisionEncrypted(context.Background(), path, sizeBytes, dek, name); err != nil {
			return Record{}, err
		}
		rec.Encrypted, rec.WrappedKey, rec.KeyID = true, wrapped, wrapKeyID
	} else if err := m.provision(path, sizeBytes); err != nil {
		return Record{}, err
	}
	if err := m.st.put(rec); err != nil {
		_ = os.Remove(path) // never leave a keyless container the backfill would mistype
		return Record{}, err
	}
	if rec.Encrypted {
		m.auditKey("volume_key_created", "volume", name, "key_id", rec.KeyID)
	}
	return rec, nil
}

// Shred crypto-shreds an encrypted volume: it destroys the LUKS keyslots
// (belt-and-suspenders) and deletes the wrapped key with the record — the only
// copy of the key — so the data is permanently unrecoverable without touching the
// ciphertext blocks. ErrInUse if a live sandbox holds it; ErrNotFound if absent;
// ErrNotEncrypted for a plaintext volume (use Remove).
func (m *Manager) Shred(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.attached[name]; ok {
		return fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	rec, ok, err := m.st.get(name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if !rec.Encrypted {
		return ErrNotEncrypted
	}
	path := filepath.Join(m.dir, name+".img")
	if m.crypt != nil {
		_ = m.crypt.Close(context.Background(), mapperName(name)) // ensure closed before erase
		if _, statErr := os.Stat(path); statErr == nil {
			if err := m.crypt.Erase(context.Background(), path); err != nil {
				return err
			}
		}
	}
	// Deleting the record removes the sole copy of the wrapped DEK.
	if err := m.st.del(name); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: remove backing file: %w", err)
	}
	m.auditKey("volume_shredded", "volume", name)
	return nil
}

// OpenDevice unlocks an encrypted volume's LUKS container and returns its
// decrypted device-mapper path (/dev/mapper/crucible-vol-<name>). The caller must
// CloseDevice when done. Host-side only — exposing the resulting node to the
// guest (staging it into the jail) is the caller's job.
// ErrNotFound if unknown; ErrNotEncrypted for a plaintext volume.
func (m *Manager) OpenDevice(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", ErrInvalidName
	}
	if m.crypt == nil {
		return "", ErrEncryptionDisabled
	}
	rec, ok, err := m.st.get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	if !rec.Encrypted {
		return "", ErrNotEncrypted
	}
	dek, err := m.dekFor(rec)
	if err != nil {
		return "", err
	}
	return m.crypt.Open(context.Background(), filepath.Join(m.dir, name+".img"), dek, mapperName(name))
}

// CloseDevice deactivates an encrypted volume's mapper node (idempotent).
func (m *Manager) CloseDevice(name string) error {
	if m.crypt == nil {
		return nil
	}
	return m.crypt.Close(context.Background(), mapperName(name))
}

// dekFor unwraps a record's per-volume key with the keyring KEK named by its
// KeyID. ErrKeyNotFound if that key was retired while the volume still used it.
func (m *Manager) dekFor(rec Record) ([]byte, error) {
	if len(rec.WrappedKey) == 0 {
		return nil, fmt.Errorf("volume: %s has no wrapped key", rec.Name)
	}
	kek, ok := m.kekFor(rec.KeyID)
	if !ok {
		return nil, fmt.Errorf("%w: %q (volume %s)", ErrKeyNotFound, rec.KeyID, rec.Name)
	}
	return cryptdev.UnwrapKey(kek, rec.WrappedKey, []byte(rec.Name))
}

// Attach claims the named volume for sandboxID and ensures its backing file
// exists (created + formatted on first use, at the recorded size — or the
// default size, auto-creating a record honoring the encryption default, when the
// volume is new). It returns the path the runner should attach and whether that
// path is an encrypted volume's decrypted device (so the jailer stages a device
// node, not a file hardlink): for a plaintext volume the backing file path; for
// an encrypted volume the /dev/mapper node from opening its LUKS container.
// ErrInUse if another live sandbox holds it; ErrEncryptionDisabled if the volume
// is encrypted but no master key is configured. The caller pairs every Attach
// with a Release, which closes the device.
func (m *Manager) Attach(name, sandboxID string) (path string, encrypted bool, err error) {
	if !nameRe.MatchString(name) {
		return "", false, ErrInvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.attached[name]; ok && holder != sandboxID {
		return "", false, fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	rec, known, err := m.st.get(name)
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(m.dir, name+".img")
	if !known {
		if rec, err = m.autoCreateLocked(name, path); err != nil {
			return "", false, err
		}
	} else if !rec.Encrypted {
		// An encrypted container already exists; only a plaintext volume needs the
		// backing file provisioned/formatted here (idempotent for an existing one).
		if err := m.provision(path, rec.SizeBytes); err != nil {
			return "", false, err
		}
	}

	if rec.Encrypted {
		if m.crypt == nil {
			return "", false, ErrEncryptionDisabled
		}
		dek, err := m.dekFor(rec)
		if err != nil {
			return "", false, err
		}
		mapper, err := m.crypt.Open(context.Background(), path, dek, mapperName(name))
		if err != nil {
			return "", false, err
		}
		m.attached[name] = sandboxID
		return mapper, true, nil
	}
	m.attached[name] = sandboxID
	return path, false, nil
}

// autoCreateLocked provisions + records a volume that Attach found no record for
// (the `run --volume name:/path` first-use path), encrypting it when the daemon
// default is on. Caller holds m.mu.
func (m *Manager) autoCreateLocked(name, path string) (Record, error) {
	size := m.defaultSize
	rec := Record{Name: name, SizeBytes: size, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}
	if m.defaultEncrypt && m.crypt != nil {
		kek, ok := m.kekFor(m.defaultKeyID)
		if !ok {
			return Record{}, fmt.Errorf("%w: %q", ErrKeyNotFound, m.defaultKeyID)
		}
		dek, err := cryptdev.NewDEK()
		if err != nil {
			return Record{}, err
		}
		wrapped, err := cryptdev.WrapKey(kek, dek, []byte(name))
		if err != nil {
			return Record{}, err
		}
		if err := m.provisionEncrypted(context.Background(), path, size, dek, name); err != nil {
			return Record{}, err
		}
		rec.Encrypted, rec.WrappedKey, rec.KeyID = true, wrapped, m.defaultKeyID
	} else if err := m.provision(path, size); err != nil {
		return Record{}, err
	}
	if err := m.st.put(rec); err != nil {
		_ = os.Remove(path)
		return Record{}, err
	}
	return rec, nil
}

// Release drops the in-memory attach claim for name and, for an encrypted volume,
// closes its decrypted device (idempotent). The backing file + record are left in
// place — volumes are durable. The caller must have stopped the VM first, so no
// open handle keeps the device busy.
func (m *Manager) Release(name string) {
	m.mu.Lock()
	delete(m.attached, name)
	m.mu.Unlock()
	if m.crypt == nil {
		return
	}
	if rec, ok, err := m.st.get(name); err == nil && ok && rec.Encrypted {
		_ = m.crypt.Close(context.Background(), mapperName(name))
	}
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

// VolumeDiskBytes returns the allocated on-disk bytes of one volume's backing
// file (sparse-aware), or 0 if the volume is unknown. Same never-fail contract
// as DiskBytes — it feeds the per-app usage accrual sampled on a tick.
func (m *Manager) VolumeDiskBytes(name string) int64 {
	return fsutil.AllocatedBytes(filepath.Join(m.dir, name+".img"))
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

// growClaim is the sentinel single-writer holder Grow installs while it resizes,
// so a concurrent Attach (a boot racing the grow) gets ErrInUse rather than
// mounting the filesystem mid-resize. It is never a real sandbox id.
const growClaim = "\x00grow"

// Grow enlarges a volume's backing store and its ext4 filesystem to newSizeBytes
// (grow-only: ErrNotLarger for a value at or below the current size). The volume
// MUST be detached — a snapshot-slept volume's guest has its block-device size
// pinned by the snapshot, so growing it offline would not be seen on wake; the
// caller (the daemon handler) refuses a grow of an attached volume. Grow claims
// the single-writer guard for the duration so a fresh Attach can't race the
// resize, and ErrInUse if the volume is already held.
//
// For a plaintext volume it sparse-truncates the backing file and runs resize2fs.
// For an encrypted volume it grows the LUKS container file, re-opens it so the
// loop device sees the new size, resizes the LUKS mapping, then grows the ext4 on
// the decrypted device. Requires resize2fs (e2fsprogs) on the host.
func (m *Manager) Grow(ctx context.Context, name string, newSizeBytes int64) (Record, error) {
	if !nameRe.MatchString(name) {
		return Record{}, ErrInvalidName
	}
	m.mu.Lock()
	if holder, ok := m.attached[name]; ok {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("%w (held by sandbox %s)", ErrInUse, holder)
	}
	rec, ok, err := m.st.get(name)
	if err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	if !ok {
		m.mu.Unlock()
		return Record{}, ErrNotFound
	}
	if newSizeBytes <= rec.SizeBytes {
		m.mu.Unlock()
		return Record{}, ErrNotLarger
	}
	// Claim the guard so nothing attaches (and mounts) the volume mid-resize.
	m.attached[name] = growClaim
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.attached, name)
		m.mu.Unlock()
	}()

	path := filepath.Join(m.dir, name+".img")
	if rec.Encrypted {
		if err := m.growEncrypted(ctx, name, path, newSizeBytes, rec); err != nil {
			return Record{}, err
		}
	} else if err := fsutil.GrowExt4(ctx, path, newSizeBytes); err != nil {
		return Record{}, fmt.Errorf("volume: grow %s: %w", name, err)
	}

	rec.SizeBytes = newSizeBytes
	m.mu.Lock()
	err = m.st.put(rec)
	m.mu.Unlock()
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// growEncrypted grows an encrypted volume: enlarge the LUKS container file (which
// holds the LUKS header plus the data area), open it fresh so cryptsetup's loop
// device picks up the new size, extend the LUKS mapping to fill it, grow the ext4
// on the decrypted mapper, then close. Caller holds the growClaim guard, so the
// mapper is not otherwise open.
func (m *Manager) growEncrypted(ctx context.Context, name, path string, newSizeBytes int64, rec Record) error {
	if m.crypt == nil {
		return ErrEncryptionDisabled
	}
	dek, err := m.dekFor(rec)
	if err != nil {
		return err
	}
	if err := os.Truncate(path, newSizeBytes+cryptdev.LUKSHeaderBytes); err != nil {
		return fmt.Errorf("volume: grow container %s: %w", name, err)
	}
	mname := mapperName(name)
	mapper, err := m.crypt.Open(ctx, path, dek, mname)
	if err != nil {
		return err
	}
	defer func() { _ = m.crypt.Close(ctx, mname) }()
	if err := m.crypt.Resize(ctx, mname, dek); err != nil {
		return err
	}
	if err := fsutil.GrowExt4(ctx, mapper, 0); err != nil {
		return fmt.Errorf("volume: grow %s: %w", name, err)
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
		Encrypted: rec.Encrypted, WrappedKey: rec.WrappedKey, KeyID: rec.KeyID,
		Kind: backupKindFull,
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

// ImportMeta carries the metadata an imported backup's bytes don't: which
// volume it came from (for the catalog), its consistency level, and whether the
// incoming stream is gzip-compressed (as ExportBackup produces by default).
type ImportMeta struct {
	SourceVolume string
	Consistency  string
	Compressed   bool
	// Kind is "incremental" to import a delta stream (Path becomes a .delta and
	// ParentID is recorded — the parent's id ON THIS HOST); empty/"full" imports a
	// whole-image .img. The caller imports a chain base-first, mapping each
	// imported id to the fresh id returned.
	Kind     string
	ParentID string
}

// ImportBackup writes an incoming backup stream into the backup dir and
// registers it, returning the new record — the inverse of OpenBackup/export.
// It places an off-host backup onto a (possibly fresh) host so RestoreTo can
// then materialise a volume from it. The record takes a freshly generated id
// and THIS host's id (the backup lives here now); a gzip stream is decompressed
// on the way in. The SourceVolume need not exist on this host (DR to a new box).
func (m *Manager) ImportBackup(meta ImportMeta, r io.Reader) (BackupRecord, error) {
	if !nameRe.MatchString(meta.SourceVolume) {
		return BackupRecord{}, ErrInvalidName
	}
	consistency := meta.Consistency
	if consistency == "" {
		consistency = "filesystem"
	}
	incremental := meta.Kind == backupKindIncremental
	if incremental && meta.ParentID == "" {
		return BackupRecord{}, fmt.Errorf("%w: incremental import needs a parent id", ErrBackupChainBroken)
	}
	id := meta.SourceVolume + "-" + time.Now().UTC().Format("20060102T150405.000Z")
	destDir := filepath.Join(m.backupDir, meta.SourceVolume)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return BackupRecord{}, fmt.Errorf("volume: create backup dir %s: %w", destDir, err)
	}
	ext := ".img"
	if incremental {
		ext = ".delta"
	}
	dst := filepath.Join(destDir, id+ext)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("volume: create backup file: %w", err)
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(dst) // never leave a partial import behind
		}
	}()

	src := r
	if meta.Compressed {
		gz, gerr := gzip.NewReader(r)
		if gerr != nil {
			return BackupRecord{}, fmt.Errorf("volume: import gunzip: %w", gerr)
		}
		defer func() { _ = gz.Close() }()
		src = gz
	}
	n, err := io.Copy(f, src)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("volume: import copy: %w", err)
	}
	if err := f.Sync(); err != nil {
		return BackupRecord{}, fmt.Errorf("volume: import fsync: %w", err)
	}
	brec := BackupRecord{
		ID: id, SourceVolume: meta.SourceVolume, SizeBytes: n,
		CreatedAt: time.Now().UTC(), Consistency: consistency, HostID: m.hostID, Path: dst,
		Kind: backupKindFull,
	}
	if incremental {
		brec.Kind, brec.ParentID = backupKindIncremental, meta.ParentID
		// SizeBytes on a delta record is the on-wire byte count, not a logical
		// volume size; the restored volume's size comes from the chain's images.
	}
	if err := m.st.putBackup(brec); err != nil {
		return BackupRecord{}, err
	}
	committed = true
	return brec, nil
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
	// Refuse to break a chain: an incremental depends on its parent's bytes.
	kids, err := m.backupChildren(rec)
	if err != nil {
		return err
	}
	if len(kids) > 0 {
		return fmt.Errorf("%w: %s (dependents: %d)", ErrBackupHasChildren, id, len(kids))
	}
	if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: remove backup file: %w", err)
	}
	if err := os.Remove(backupManifestPath(rec.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: remove backup manifest: %w", err)
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
	var enc *rewrapSrc
	if brec.Encrypted {
		enc = &rewrapSrc{sourceName: brec.SourceVolume, wrappedKey: brec.WrappedKey, keyID: brec.KeyID}
	}
	// A full backup materializes straight from its .img (the v0.6.3 fast path). An
	// incremental first reassembles its chain (base full + each delta) into a temp
	// image, then materializes from that.
	if brec.Kind != backupKindIncremental {
		return m.materialize(newName, brec.Path, brec.SizeBytes, enc)
	}
	chain, err := m.resolveChain(backupID)
	if err != nil {
		return Record{}, err
	}
	tmpPath := filepath.Join(m.dir, newName+".reconstruct.tmp")
	_ = os.Remove(tmpPath)
	defer func() { _ = os.Remove(tmpPath) }()
	if err := m.reconstructChain(chain, tmpPath); err != nil {
		return Record{}, err
	}
	// Derive the logical size from the reconstructed image (an imported delta's
	// record SizeBytes is the wire byte count, not the volume size): the image is
	// the ext4 file, or the LUKS container (logical = container - header).
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return Record{}, err
	}
	logical := fi.Size()
	if brec.Encrypted {
		logical -= cryptdev.LUKSHeaderBytes
	}
	return m.materialize(newName, tmpPath, logical, enc)
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
	// An encrypted clone must get a FRESH key (no shared ciphertext lineage), which
	// is a full decrypt-copy-encrypt, not the reflink copy below — refuse rather
	// than silently produce a same-key clone. (Restore, by contrast, recreates the
	// SAME volume and correctly re-wraps the same key.)
	if srcRec.Encrypted {
		return Record{}, ErrEncryptedCloneUnsupported
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
	return m.materialize(dst, filepath.Join(m.dir, src+".img"), srcRec.SizeBytes, nil)
}

// rewrapSrc carries an encrypted source's key material so materialize can re-wrap
// it under the new volume's name (the AAD). keyID names the keyring key that both
// wrapped the source and will wrap the copy (restore keeps the source's key).
// nil for a plaintext source.
type rewrapSrc struct {
	sourceName string
	wrappedKey []byte
	keyID      string
}

// materialize copies srcPath into a new volume named name (its backing file +
// record). For an encrypted source (enc != nil) the copied LUKS container opens
// with the same per-volume key; materialize re-wraps that key under the new name
// (only the wrapping AAD changes) and leaves the container root-owned. A plaintext
// copy is chowned so a jailed firecracker can open the file directly. Caller holds
// m.mu and has verified name is free.
func (m *Manager) materialize(name, srcPath string, sizeBytes int64, enc *rewrapSrc) (Record, error) {
	dstPath := filepath.Join(m.dir, name+".img")
	if err := fsutil.Clone(srcPath, dstPath); err != nil {
		return Record{}, fmt.Errorf("volume: materialize %s: %w", name, err)
	}
	rec := Record{Name: name, SizeBytes: sizeBytes, CreatedAt: time.Now().UTC(), Formatted: true, HostID: m.hostID}
	if enc != nil {
		if m.crypt == nil {
			_ = os.Remove(dstPath)
			return Record{}, ErrEncryptionDisabled
		}
		kek, ok := m.kekFor(enc.keyID)
		if !ok {
			_ = os.Remove(dstPath)
			return Record{}, fmt.Errorf("%w: %q (source %s)", ErrKeyNotFound, enc.keyID, enc.sourceName)
		}
		dek, err := cryptdev.UnwrapKey(kek, enc.wrappedKey, []byte(enc.sourceName))
		if err != nil {
			_ = os.Remove(dstPath)
			return Record{}, fmt.Errorf("volume: unwrap source key: %w", err)
		}
		wrapped, err := cryptdev.WrapKey(kek, dek, []byte(name))
		if err != nil {
			_ = os.Remove(dstPath)
			return Record{}, err
		}
		rec.Encrypted, rec.WrappedKey, rec.KeyID = true, wrapped, enc.keyID
	} else if err := os.Chown(dstPath, m.uid, m.gid); err != nil {
		_ = os.Remove(dstPath)
		return Record{}, fmt.Errorf("volume: chown %s: %w", dstPath, err)
	}
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

// provisionEncrypted creates a LUKS2 container at path and formats ext4 on its
// decrypted device. The file is sized sizeBytes + the LUKS header so the guest
// sees the full requested capacity. Like provision, it formats a temp file
// renamed only on success (a crash mid-format leaves no half-formatted file the
// "exists ⇒ provisioned" check would trust) and is idempotent. The container
// stays root-owned 0600 — only the daemon opens it; the guest gets the mapper
// node, never the ciphertext file. Caller holds m.mu.
func (m *Manager) provisionEncrypted(ctx context.Context, path string, sizeBytes int64, dek []byte, vol string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("volume: stat %s: %w", path, err)
	}

	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("volume: create %s: %w", tmp, err)
	}
	if err := f.Truncate(sizeBytes + cryptdev.LUKSHeaderBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: size %s: %w", tmp, err)
	}
	_ = f.Close()

	if err := m.crypt.Format(ctx, tmp, dek); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: %w", err)
	}
	mname := fmtMapperName(vol)
	mapper, err := m.crypt.Open(ctx, tmp, dek, mname)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: %w", err)
	}
	mkfsErr := mkfsExt4(mapper)
	_ = m.crypt.Close(ctx, mname) // always release the mapper + loop device
	if mkfsErr != nil {
		_ = os.Remove(tmp)
		return mkfsErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("volume: finalize %s: %w", path, err)
	}
	return nil
}

// luksMagic is the 6-byte signature at the start of every LUKS1/LUKS2 header.
var luksMagic = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}

// looksLikeLUKS reports whether path begins with the LUKS header magic, so
// backfill can recognise an encrypted container without the crypt engine.
func looksLikeLUKS(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var buf [6]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return false
	}
	for i := range luksMagic {
		if buf[i] != luksMagic[i] {
			return false
		}
	}
	return true
}

// mkfsExt4 formats dev (a plain file or a decrypted mapper device) ext4.
func mkfsExt4(dev string) error {
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", "-m", "0", dev).CombinedOutput(); err != nil {
		return fmt.Errorf("volume: mkfs.ext4 %s: %w: %s", dev, err, string(out))
	}
	return nil
}
