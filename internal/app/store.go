package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/gnana997/crucible/sdk/api"
)

// Record is the persisted desired state of one app. Only desired state is
// durable: the observed status (running instance, health, restart count)
// is derived at runtime by the reconciler and never written here, so the
// store is a small, slow-changing control-plane truth — distinct from the
// high-rate sandbox lifecycle journal (internal/sandbox/store.go).
type Record struct {
	// ID is the app id (app_...). Also the store key.
	ID string `json:"id"`

	// Spec is the full desired workload. Spec.Name is the unique
	// user-facing handle.
	Spec api.AppSpec `json:"spec"`

	// DesiredRunning is true when the daemon should keep an instance
	// alive, false when the app is stopped (spec retained, no instance).
	DesiredRunning bool `json:"desired_running"`

	// Generation increments on each spec update so the reconciler can
	// detect a change that needs a redeploy.
	Generation uint64 `json:"generation"`

	// AsleepSnapshotID, when non-empty, means the app is asleep (scale-to-zero):
	// its instance's VMM is stopped and this durable snapshot holds its warm
	// state. Persisted so a daemon restart re-adopts the app as asleep (and wakes
	// it from this snapshot) instead of cold-booting. Cleared on wake.
	AsleepSnapshotID string `json:"asleep_snapshot_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// bucket holds every app record, keyed by app id.
var appsBucket = []byte("apps")

// usageBucket holds the durable per-app usage counters, keyed by app id. Kept
// in the same bbolt file as the app records so it rides the existing consistent
// backup (BackupTo / `admin backup`). See usage.go for the accrual model.
var usageBucket = []byte("usage")

// Store is a durable, transactional app record store backed by a single
// bbolt file. bbolt is chosen over SQLite deliberately: pure Go (no cgo,
// keeping the static-build discipline), single file, no server, and a
// real transactional KV — the simplest thing that is actually durable.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if absent) the app store at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("app: open store %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists(appsBucket); e != nil {
			return e
		}
		_, e := tx.CreateBucketIfNotExists(usageBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: init store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the store's file lock.
func (s *Store) Close() error { return s.db.Close() }

// BackupTo streams a consistent point-in-time copy of the store's bbolt file
// (a read transaction pins the snapshot while the daemon keeps serving).
// frame receives the exact byte count — from the same transaction, so
// tar-style callers can write a correct header — and returns the destination.
func (s *Store) BackupTo(frame func(size int64) (io.Writer, error)) error {
	return s.db.View(func(tx *bolt.Tx) error {
		w, err := frame(tx.Size())
		if err != nil {
			return err
		}
		_, err = tx.WriteTo(w)
		return err
	})
}

// Put upserts a record by its ID.
func (s *Store) Put(rec Record) error {
	if rec.ID == "" {
		return errors.New("app: Put: empty id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("app: marshal record: %w", err)
		}
		return tx.Bucket(appsBucket).Put([]byte(rec.ID), b)
	})
}

// Get returns the record for id. The bool is false when no such app
// exists (not an error).
func (s *Store) Get(id string) (Record, bool, error) {
	var rec Record
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(appsBucket).Get([]byte(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// GetByName returns the record whose Spec.Name matches name. Names are
// unique (enforced by the caller under a lock); this is a scan, which is
// fine at the app-count scale of a single node.
func (s *Store) GetByName(name string) (Record, bool, error) {
	var rec Record
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(appsBucket).ForEach(func(_, v []byte) error {
			if found {
				return nil
			}
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.Spec.Name == name {
				rec, found = r, true
			}
			return nil
		})
	})
	return rec, found, err
}

// List returns every record, unordered.
func (s *Store) List() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(appsBucket).ForEach(func(_, v []byte) error {
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

// Delete removes the record for id. Absent id is not an error. The usage
// record is intentionally NOT removed here: a deleted app's final usage is
// retained (finalized) so a control plane can still read it. See PruneUsage.
func (s *Store) Delete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(appsBucket).Delete([]byte(id))
	})
}

// PutUsage upserts an app's usage counters by app id.
func (s *Store) PutUsage(id string, u Usage) error {
	if id == "" {
		return errors.New("app: PutUsage: empty id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("app: marshal usage: %w", err)
		}
		return tx.Bucket(usageBucket).Put([]byte(id), b)
	})
}

// GetUsage returns the usage counters for app id. The bool is false when no
// usage record exists yet (not an error).
func (s *Store) GetUsage(id string) (Usage, bool, error) {
	var u Usage
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(usageBucket).Get([]byte(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &u)
	})
	return u, found, err
}

// ListUsage returns every app's usage counters keyed by app id, unordered.
func (s *Store) ListUsage() (map[string]Usage, error) {
	out := make(map[string]Usage)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(usageBucket).ForEach(func(k, v []byte) error {
			var u Usage
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			out[string(k)] = u
			return nil
		})
	})
	return out, err
}

// DeleteUsage removes an app's usage record (used to reclaim a finalized
// record, e.g. when a name is reused by a fresh app). Absent id is not an error.
func (s *Store) DeleteUsage(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(usageBucket).Delete([]byte(id))
	})
}

// PruneUsage deletes FINALIZED usage records whose FinalizedAt is before cutoff and
// reports how many were removed. A deleted app's record is retained so a reader can
// still collect its final counters (see Delete); this is what eventually reclaims them,
// so the bucket does not grow without bound under high app churn.
//
// Records for LIVE apps are never touched, at any age: an app's usage record IS its
// running counter, so removing it would silently reset the app's lifetime totals rather
// than free anything. FinalizedAt == nil means "still live" — that check is the whole
// safety property of this function.
//
// Safe to call concurrently with reads, idempotent, and a no-op on an empty bucket.
func (s *Store) PruneUsage(cutoff time.Time) (int, error) {
	// Collect first, delete second: bbolt keys are only valid for the life of the
	// transaction that yielded them, and mutating a bucket while ranging it is
	// undefined. Copy each key out before leaving the View.
	var stale [][]byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(usageBucket).ForEach(func(k, v []byte) error {
			var u Usage
			if err := json.Unmarshal(v, &u); err != nil {
				// A record we cannot parse is not one we can judge finalized, so leave
				// it: a janitor must not abort the whole sweep over a single bad row.
				return nil
			}
			if u.FinalizedAt != nil && u.FinalizedAt.Before(cutoff) {
				stale = append(stale, append([]byte(nil), k...))
			}
			return nil
		})
	}); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(usageBucket)
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(stale), nil
}
