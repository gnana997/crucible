package volume

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Record is the persisted metadata of one volume, keyed by Name. Durable so
// `volume ls` survives a daemon restart and `run --volume` honours a volume's
// recorded size. Attachment is NOT persisted — it is live-only (the in-memory
// single-writer guard), because no live sandbox survives a daemon restart.
type Record struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Formatted bool      `json:"formatted"` // true once mkfs has succeeded on the backing file
	HostID    string    `json:"host_id"`   // host-pin for future multi-host placement; recorded at create
	// Encrypted marks a LUKS2-encrypted volume: its backing file is ciphertext and
	// it opens through a device-mapper node keyed by WrappedKey. omitempty keeps a
	// plaintext record (and every pre-v0.8.0 record) byte-identical.
	Encrypted bool `json:"encrypted,omitempty"`
	// WrappedKey is the per-volume DEK sealed under the daemon master KEK
	// (cryptdev.WrapKey, AAD=Name) — never the plaintext key. It is the ONLY copy
	// of the key material; deleting the record crypto-shreds the volume. Empty
	// unless Encrypted.
	WrappedKey []byte `json:"wrapped_key,omitempty"`
	// KeyID names which master KEK wrapped WrappedKey, for future key rotation.
	KeyID string `json:"key_id,omitempty"`
}

// BackupRecord is the persisted metadata of one volume backup, keyed by ID. A
// backup is a point-in-time copy of a volume's backing file (see Manager.Backup),
// restorable to a new volume. Kept in its own bucket because a volume has many
// backups, each with its own id, timestamp, and consistency level.
type BackupRecord struct {
	ID           string    `json:"id"`            // "<source>-<UTC-compact-ms>"
	SourceVolume string    `json:"source_volume"` // the volume this was taken from
	SizeBytes    int64     `json:"size_bytes"`    // logical size (same as the source)
	CreatedAt    time.Time `json:"created_at"`
	Consistency  string    `json:"consistency"` // "filesystem" (detached/slept/frozen)
	HostID       string    `json:"host_id"`
	Path         string    `json:"path"` // host path of the backup backing file (internal)
	// Encrypted marks a backup of an encrypted volume: Path is the LUKS container
	// (ciphertext), and WrappedKey carries the source volume's per-volume key so
	// RestoreTo can re-wrap it under the restored name. Empty unless Encrypted.
	Encrypted  bool   `json:"encrypted,omitempty"`
	WrappedKey []byte `json:"wrapped_key,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
}

var (
	volumesBucket = []byte("volumes")
	backupsBucket = []byte("backups")
)

// store is a durable, transactional volume-record store backed by a single
// bbolt file — same shape as internal/app/store.go (pure Go, single file,
// transactional KV). Unexported: the Manager is the only user.
type store struct {
	db *bolt.DB
}

func openStore(path string) (*store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("volume: open store %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists(volumesBucket); e != nil {
			return e
		}
		_, e := tx.CreateBucketIfNotExists(backupsBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("volume: init store: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error { return s.db.Close() }

// backupTo streams a consistent point-in-time copy of the store's bbolt file;
// same contract as internal/app Store.BackupTo (frame gets the exact size from
// the pinning read transaction and returns the destination writer).
func (s *store) backupTo(frame func(size int64) (io.Writer, error)) error {
	return s.db.View(func(tx *bolt.Tx) error {
		w, err := frame(tx.Size())
		if err != nil {
			return err
		}
		_, err = tx.WriteTo(w)
		return err
	})
}

func (s *store) put(rec Record) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Bucket(volumesBucket).Put([]byte(rec.Name), b)
	})
}

func (s *store) get(name string) (Record, bool, error) {
	var rec Record
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(volumesBucket).Get([]byte(name))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

func (s *store) list() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(volumesBucket).ForEach(func(_, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *store) del(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(volumesBucket).Delete([]byte(name))
	})
}

func (s *store) putBackup(rec BackupRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Bucket(backupsBucket).Put([]byte(rec.ID), b)
	})
}

func (s *store) getBackup(id string) (BackupRecord, bool, error) {
	var rec BackupRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(backupsBucket).Get([]byte(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// listBackups returns every backup record; the Manager filters by source volume.
func (s *store) listBackups() ([]BackupRecord, error) {
	var out []BackupRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(backupsBucket).ForEach(func(_, v []byte) error {
			var r BackupRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *store) delBackup(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(backupsBucket).Delete([]byte(id))
	})
}
