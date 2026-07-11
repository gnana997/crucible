package app

import (
	"encoding/json"
	"errors"
	"fmt"
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

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// bucket holds every app record, keyed by app id.
var appsBucket = []byte("apps")

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
		_, e := tx.CreateBucketIfNotExists(appsBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: init store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the store's file lock.
func (s *Store) Close() error { return s.db.Close() }

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

// Delete removes the record for id. Absent id is not an error.
func (s *Store) Delete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(appsBucket).Delete([]byte(id))
	})
}
