package tokenstore

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var tokensBucket = []byte("tokens")

// BoltStore is a bbolt-backed token store: durable, transactional, and safe to
// mutate at runtime (mint/revoke) while the daemon serves requests, unlike the
// read-modify-write JSON file store. Entries are persisted keyed by id; an
// in-memory hash→entry index makes Identify O(1) on the request hot path.
//
// Single-writer: the daemon process owns the bbolt file, so token management is
// daemon-mediated (an HTTP endpoint) rather than an out-of-process CLI file
// edit. It satisfies the same read side (Identify/Verify/Enabled) as the file
// Store, plus runtime Create/List/Revoke.
type BoltStore struct {
	db *bolt.DB

	mu     sync.RWMutex
	byHash map[string]Entry // hash → entry (hot-path lookup cache)
}

// OpenBolt opens (creating if absent) a bbolt token store at path and loads its
// in-memory index. Close it on shutdown to release the file lock.
func OpenBolt(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("tokenstore: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(tokensBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("tokenstore: init %s: %w", path, err)
	}
	s := &BoltStore{db: db, byHash: map[string]Entry{}}
	if err := s.loadIndex(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the store's file lock.
func (s *BoltStore) Close() error { return s.db.Close() }

func (s *BoltStore) loadIndex() error {
	m := map[string]Entry{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(tokensBucket).ForEach(func(_, v []byte) error {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			m[e.Hash] = e
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("tokenstore: load index: %w", err)
	}
	s.mu.Lock()
	s.byHash = m
	s.mu.Unlock()
	return nil
}

// Create mints a new key per opts, persists its hashed record, updates the
// index, and returns the raw key once — the caller must show it to the operator
// and never persist it.
func (s *BoltStore) Create(opts AddOptions) (rawKey string, e Entry, err error) {
	raw, err := randToken(24, keyPrefix)
	if err != nil {
		return "", Entry{}, err
	}
	id, err := randHex(4)
	if err != nil {
		return "", Entry{}, err
	}
	e = Entry{ID: id, Name: opts.Name, Hash: hashKey(raw), CreatedAt: time.Now().UTC(), Policy: opts.Policy}
	if opts.TTL != 0 {
		exp := e.CreatedAt.Add(opts.TTL)
		e.ExpiresAt = &exp
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return "", Entry{}, err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(tokensBucket).Put([]byte(e.ID), buf)
	}); err != nil {
		return "", Entry{}, fmt.Errorf("tokenstore: create: %w", err)
	}
	s.mu.Lock()
	s.byHash[e.Hash] = e
	s.mu.Unlock()
	return raw, e, nil
}

// List returns all entries (no secrets).
func (s *BoltStore) List() ([]Entry, error) {
	var out []Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(tokensBucket).ForEach(func(_, v []byte) error {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// Revoke deletes the entry with id and evicts it from the index. Returns false
// if no such id exists.
func (s *BoltStore) Revoke(id string) (bool, error) {
	var hash string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(tokensBucket)
		v := b.Get([]byte(id))
		if v == nil {
			return nil
		}
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		hash = e.Hash
		return b.Delete([]byte(id))
	})
	if err != nil {
		return false, fmt.Errorf("tokenstore: revoke: %w", err)
	}
	if hash == "" {
		return false, nil
	}
	s.mu.Lock()
	delete(s.byHash, hash)
	s.mu.Unlock()
	return true, nil
}

// Enabled reports whether any keys exist (i.e. whether auth is required).
func (s *BoltStore) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byHash) > 0
}

// Identify verifies key and returns the caller's identity. ok is false when the
// key is unknown or expired. The returned Policy is read-only.
func (s *BoltStore) Identify(key string) (Identity, bool) {
	if key == "" {
		return Identity{}, false
	}
	s.mu.RLock()
	e, found := s.byHash[hashKey(key)]
	s.mu.RUnlock()
	if !found || e.Expired(time.Now()) {
		return Identity{}, false
	}
	return Identity{TokenID: e.ID, Policy: e.Policy}, true
}

// Verify reports whether key matches a stored, unexpired token.
func (s *BoltStore) Verify(key string) bool {
	_, ok := s.Identify(key)
	return ok
}
