package volume

import (
	"encoding/json"
	"fmt"
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
	HostID    string    `json:"host_id"`   // host-pin (P5 placement); recorded at create
}

var volumesBucket = []byte("volumes")

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
		_, e := tx.CreateBucketIfNotExists(volumesBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("volume: init store: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error { return s.db.Close() }

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
