// Package registryauth manages the daemon's private-registry credentials — a
// small JSON file of per-registry (username, secret) pairs, added and removed
// over the daemon API (`crucible registry login/logout`) and fed to
// go-containerregistry's authn when pulling an image.
//
// Unlike the token store, the file holds usable secrets (a registry password
// or token can't be hashed — it must be replayed to the registry), so it is
// written 0600 and treated as sensitive: secrets are never logged, never
// returned by List, and redacted from errors. Documented as NOT encrypted at
// rest, the same posture as any credential file (e.g. ~/.docker/config.json).
package registryauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// Cred is one registry's stored credential.
type Cred struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"created_at"`
}

type fileFormat struct {
	Registries []Cred `json:"registries"`
}

// canonicalHost folds a registry hostname to the key ggcr resolves against, so
// a login matches the pull. It lowercases, strips a pasted scheme/trailing
// slash, and maps Docker Hub's aliases (docker.io, registry-1.docker.io, …) to
// name.DefaultRegistry ("index.docker.io") — the host ggcr reports for a
// docker.io image.
func canonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/")
	switch h {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return name.DefaultRegistry
	}
	return h
}

// Store is a lazily-loaded, mtime-refreshed view of the credential file. It
// mirrors tokenstore: writes go through a temp-file rename so a concurrent read
// never sees a partial file, and the cache reloads when the file changes.
type Store struct {
	path string

	mu     sync.Mutex
	loaded bool
	mtime  time.Time
	size   int64
	creds  map[string]Cred // canonicalHost → cred
}

// Open returns a Store backed by path (read lazily on first use). An empty path
// is a disabled store: reads return nothing and writes error, so pulls fall
// back to anonymous.
func Open(path string) *Store { return &Store{path: path} }

func readCreds(path string) ([]Cred, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse registry credential file %s: %w", path, err)
	}
	return f.Registries, nil
}

func writeCreds(path string, creds []Cred) error {
	b, err := json.MarshalIndent(fileFormat{Registries: creds}, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename; 0600 because this file holds usable secrets. Remove the
	// temp file on a failed write so a partial secret never lingers on disk
	// (e.g. a mid-write ENOSPC) under an unexpected name.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// loadLocked refreshes the cache from disk when the file has changed. Caller
// holds mu. Transient stat/parse errors keep the last-known-good state.
func (s *Store) loadLocked() {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if !s.loaded {
			s.creds, s.loaded = map[string]Cred{}, true
		}
		return
	}
	if err != nil {
		return
	}
	if s.loaded && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size {
		return
	}
	creds, err := readCreds(s.path)
	if err != nil {
		return
	}
	m := make(map[string]Cred, len(creds))
	for _, c := range creds {
		m[canonicalHost(c.Host)] = c
	}
	s.creds, s.loaded, s.mtime, s.size = m, true, fi.ModTime(), fi.Size()
}

// saveLocked writes the current creds and refreshes the stat cache so the next
// load no-ops. Caller holds mu.
func (s *Store) saveLocked() error {
	creds := make([]Cred, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	sort.Slice(creds, func(i, j int) bool { return creds[i].Host < creds[j].Host })
	if err := writeCreds(s.path, creds); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime, s.size = fi.ModTime(), fi.Size()
	}
	return nil
}

// Upsert stores (or replaces) the credential for host. username may be empty
// for registries that authenticate on the secret alone; secret is required.
func (s *Store) Upsert(host, username, secret string) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("registryauth: host is required")
	}
	if secret == "" {
		return errors.New("registryauth: secret is required")
	}
	if s.path == "" {
		return errors.New("registryauth: no credential store configured")
	}
	key := canonicalHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.creds[key] = Cred{Host: key, Username: username, Secret: secret, CreatedAt: time.Now().UTC()}
	return s.saveLocked()
}

// Delete removes the credential for host, reporting whether one existed.
func (s *Store) Delete(host string) (bool, error) {
	if s.path == "" {
		return false, errors.New("registryauth: no credential store configured")
	}
	key := canonicalHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if _, ok := s.creds[key]; !ok {
		return false, nil
	}
	delete(s.creds, key)
	return true, s.saveLocked()
}

// List returns the stored credentials with their secrets zeroed — safe to log
// or return over the API.
func (s *Store) List() []Cred {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make([]Cred, 0, len(s.creds))
	for _, c := range s.creds {
		c.Secret = ""
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Lookup returns the full credential (secret included) for host, canonicalized.
// Used by the keychain at pull time; never expose the result to a client.
func (s *Store) Lookup(host string) (Cred, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	c, ok := s.creds[canonicalHost(host)]
	return c, ok
}

// Keychain returns an authn.Keychain that resolves a registry host to its
// stored credential, falling back to anonymous when none is configured — so
// public images keep pulling. A nil *Store yields an always-anonymous keychain.
func (s *Store) Keychain() authn.Keychain { return keychain{s} }

type keychain struct{ s *Store }

func (k keychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k.s == nil {
		return authn.Anonymous, nil
	}
	if c, ok := k.s.Lookup(res.RegistryStr()); ok {
		return authn.FromConfig(authn.AuthConfig{Username: c.Username, Password: c.Secret}), nil
	}
	return authn.Anonymous, nil
}
