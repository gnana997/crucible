// Package tokenstore manages the daemon's API keys — a small JSON file of
// hashed bearer keys, generated and revoked by `crucible daemon token …`
// and verified by the daemon on each request.
//
// Keys are stored as SHA-256 hashes, so the file never contains a usable
// secret: a leaked token file yields no working keys. The raw key is shown
// to the operator exactly once, at creation.
package tokenstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/policy"
)

// keyPrefix makes a crucible key recognizable (e.g. in logs/leaks it's
// obvious what it is) without revealing anything.
const keyPrefix = "crucible_"

// Entry is one API key's public record — never the secret itself.
type Entry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // hex SHA-256 of the raw key
	CreatedAt time.Time `json:"created_at"`

	// ExpiresAt, when set, is the absolute instant after which the key is
	// rejected. Nil = never expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Policy, when set, bounds what the key may do (the daemon enforces it).
	// Nil = unscoped: full access, the v0.1.2 behavior.
	Policy *policy.Policy `json:"policy,omitempty"`
}

// Expired reports whether the key has passed its expiry as of now.
func (e Entry) Expired(now time.Time) bool {
	return e.ExpiresAt != nil && now.After(*e.ExpiresAt)
}

// Scoped reports whether the key carries a policy (vs full-access unscoped).
func (e Entry) Scoped() bool { return e.Policy != nil }

type fileFormat struct {
	Tokens []Entry `json:"tokens"`
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func readEntries(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse token file %s: %w", path, err)
	}
	return f.Tokens, nil
}

func writeEntries(path string, entries []Entry) error {
	b, err := json.MarshalIndent(fileFormat{Tokens: entries}, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a concurrent daemon read never sees a partial
	// file. 0600 — the contents are hashes, not secrets, but keep it tight.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AddOptions configures a new key. The zero value mints an unscoped,
// non-expiring key (the v0.1.2 behavior).
type AddOptions struct {
	Name   string
	Policy *policy.Policy // nil = unscoped (full access)
	TTL    time.Duration  // 0 = never expires
}

// Add generates a new key, appends its hashed record per opts, and returns the
// raw key — which the caller must show to the operator once and never persist.
func Add(path string, opts AddOptions) (rawKey string, e Entry, err error) {
	entries, err := readEntries(path)
	if err != nil {
		return "", Entry{}, err
	}
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
	if err := writeEntries(path, append(entries, e)); err != nil {
		return "", Entry{}, err
	}
	return raw, e, nil
}

// List returns the stored entries (no secrets).
func List(path string) ([]Entry, error) { return readEntries(path) }

// Revoke removes the entry with id. Returns false if no such id exists.
func Revoke(path, id string) (bool, error) {
	entries, err := readEntries(path)
	if err != nil {
		return false, err
	}
	out := make([]Entry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return false, nil
	}
	return true, writeEntries(path, out)
}

func randToken(nbytes int, prefix string) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func randHex(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// --- runtime store (daemon side) -------------------------------------

// Store is the daemon's live view of the token file. It reloads when the
// file's mtime changes, so `token add`/`revoke` take effect without a
// daemon restart.
type Store struct {
	path string

	mu      sync.Mutex
	loaded  bool
	mtime   time.Time
	size    int64
	entries map[string]Entry // hash → entry (carries policy + expiry)
}

// Open returns a Store backed by path. The file is read lazily on first use.
func Open(path string) *Store { return &Store{path: path} }

// Dump returns the raw bytes of the token file for the control-plane backup,
// read under the store lock. A missing file (no tokens yet) dumps as empty,
// not an error — the backup just omits it. Contents are key HASHES plus
// policies, so a backup restores auth state without holding usable secrets.
func (s *Store) Dump() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (s *Store) reload() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		s.entries = map[string]Entry{}
		return
	}
	if err != nil {
		return // keep last-known-good state on a transient stat error
	}
	// mtime + size, so an add/revoke within the same mtime tick (coarse-
	// granularity filesystems) still triggers a reload — the size changes.
	if s.loaded && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size {
		return
	}
	entries, err := readEntries(s.path)
	if err != nil {
		return
	}
	m := make(map[string]Entry, len(entries))
	for _, e := range entries {
		m[e.Hash] = e
	}
	s.loaded, s.mtime, s.size, s.entries = true, fi.ModTime(), fi.Size(), m
}

// Enabled reports whether any keys exist — i.e. whether auth is required.
func (s *Store) Enabled() bool {
	s.reload()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) > 0
}

// Identity is the verified caller behind an API key: the key's id (for
// attribution, e.g. per-token quotas) and its policy (nil for an unscoped key).
type Identity struct {
	TokenID string
	Policy  *policy.Policy
}

// Identify verifies key and returns the caller's identity. ok is false when the
// key is unknown or expired. The returned Policy is for the caller to read only
// — do not mutate it.
func (s *Store) Identify(key string) (Identity, bool) {
	if key == "" {
		return Identity{}, false
	}
	s.reload()
	s.mu.Lock()
	e, found := s.entries[hashKey(key)]
	s.mu.Unlock()
	if !found || e.Expired(time.Now()) {
		return Identity{}, false
	}
	return Identity{TokenID: e.ID, Policy: e.Policy}, true
}

// Verify reports whether key matches a stored, unexpired token.
func (s *Store) Verify(key string) bool {
	_, ok := s.Identify(key)
	return ok
}
