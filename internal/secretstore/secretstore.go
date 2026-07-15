// Package secretstore is the daemon's encrypted secret store. A secret is a
// named BUNDLE of key→value pairs (the Kubernetes-Secret model): a whole `.env`
// becomes one uniquely-named bundle, and an app injects every key of a bundle as
// environment variables (envFrom). Each bundle is sealed at rest with
// AES-256-GCM under the daemon master key, with the bundle NAME as the AEAD's
// additional data — so a ciphertext moved to a different name fails to open, and
// GCM's tag catches any tampering. The store never returns a plaintext value over
// any public path; only the in-process injection reads bundles back.
//
// Unlike internal/registryauth (write-only but plaintext at rest), this store IS
// encrypted at rest — the point of the feature. The master key lives outside the
// store (a key file or env), so a stolen store file (or `admin backup`) is inert.
package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrNotFound is returned when a bundle does not exist.
var ErrNotFound = errors.New("secretstore: not found")

// KeySize is the master key length (AES-256).
const KeySize = 32

var bundlesBucket = []byte("secrets")

// validName bounds a bundle name (a bbolt key + user-facing handle): a DNS-ish
// label, so it's safe and predictable.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether name is an acceptable bundle name.
func ValidName(name string) bool { return validName.MatchString(name) }

// sealed is the persisted form of one bundle: the AES-256-GCM nonce + ciphertext
// of the JSON-serialized key→value map, plus timestamps (which are NOT secret and
// help auditing / `admin backup`).
type sealed struct {
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ct"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

// Store is a bbolt-backed, AES-256-GCM-encrypted bundle store.
type Store struct {
	db   *bolt.DB
	aead cipher.AEAD
	now  func() time.Time
}

// Open opens (creating if absent) the store at path, sealing with the given
// 32-byte master key.
func Open(path string, key []byte) (*Store, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretstore: master key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretstore: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretstore: gcm: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("secretstore: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bundlesBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secretstore: init: %w", err)
	}
	return &Store{db: db, aead: aead, now: time.Now}, nil
}

// Close releases the store's file lock.
func (s *Store) Close() error { return s.db.Close() }

// seal encrypts a bundle map under name (AAD).
func (s *Store) seal(name string, data map[string]string) (sealed, error) {
	plain, err := json.Marshal(data)
	if err != nil {
		return sealed{}, fmt.Errorf("secretstore: marshal bundle: %w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return sealed{}, fmt.Errorf("secretstore: nonce: %w", err)
	}
	ct := s.aead.Seal(nil, nonce, plain, []byte(name))
	return sealed{Nonce: nonce, Ciphertext: ct}, nil
}

// unseal decrypts a bundle sealed under name.
func (s *Store) unseal(name string, rec sealed) (map[string]string, error) {
	plain, err := s.aead.Open(nil, rec.Nonce, rec.Ciphertext, []byte(name))
	if err != nil {
		return nil, fmt.Errorf("secretstore: open bundle %q (wrong key, tampered, or wrong name): %w", name, err)
	}
	var data map[string]string
	if err := json.Unmarshal(plain, &data); err != nil {
		return nil, fmt.Errorf("secretstore: unmarshal bundle: %w", err)
	}
	return data, nil
}

// Set creates or replaces a bundle with exactly data.
func (s *Store) Set(name string, data map[string]string) error {
	if !ValidName(name) {
		return fmt.Errorf("secretstore: invalid bundle name %q (want a DNS label: lowercase alphanumeric + hyphens)", name)
	}
	if data == nil {
		data = map[string]string{}
	}
	rec, err := s.seal(name, data)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	rec.Created, rec.Updated = now, now
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bundlesBucket)
		if old := b.Get([]byte(name)); old != nil { // preserve original Created
			var prev sealed
			if json.Unmarshal(old, &prev) == nil && !prev.Created.IsZero() {
				rec.Created = prev.Created
			}
		}
		enc, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(name), enc)
	})
}

// SetKeys merges kv into a bundle (creating it if absent), leaving other keys
// intact.
func (s *Store) SetKeys(name string, kv map[string]string) error {
	data, _, err := s.Get(name)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]string{}
	}
	for k, v := range kv {
		data[k] = v
	}
	return s.Set(name, data)
}

// DeleteKey removes one key from a bundle. Absent bundle → ErrNotFound; absent
// key is not an error.
func (s *Store) DeleteKey(name, key string) error {
	data, found, err := s.Get(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	delete(data, key)
	return s.Set(name, data)
}

// Delete removes a bundle entirely. Absent name is not an error.
func (s *Store) Delete(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bundlesBucket).Delete([]byte(name))
	})
}

// Get returns a bundle's DECRYPTED key→value map. In-process only (injection);
// no HTTP path calls this. The bool is false when the bundle does not exist.
func (s *Store) Get(name string) (map[string]string, bool, error) {
	var rec sealed
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bundlesBucket).Get([]byte(name))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil || !found {
		return nil, found, err
	}
	data, err := s.unseal(name, rec)
	return data, true, err
}

// Exists reports whether a bundle is present (no decryption).
func (s *Store) Exists(name string) bool {
	var ok bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bundlesBucket).Get([]byte(name)) != nil
		return nil
	})
	return ok
}

// List returns every bundle name, sorted.
func (s *Store) List() ([]string, error) {
	var names []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bundlesBucket).ForEach(func(k, _ []byte) error {
			names = append(names, string(k))
			return nil
		})
	})
	sort.Strings(names)
	return names, err
}

// Keys returns a bundle's key NAMES (sorted) — never its values. The bool is
// false when the bundle does not exist.
func (s *Store) Keys(name string) ([]string, bool, error) {
	data, found, err := s.Get(name)
	if err != nil || !found {
		return nil, found, err
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true, nil
}
