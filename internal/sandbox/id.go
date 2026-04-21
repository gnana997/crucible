package sandbox

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// Identifier prefixes — every resource carries its type on the wire so
// `sbx_...` vs `snap_...` is self-identifying in logs, paths, and URLs.
const (
	sandboxIDPrefix  = "sbx_"
	snapshotIDPrefix = "snap_"
)

// idRandomBytes is the number of random bytes hashed into each ID. 8 bytes
// = 64 bits of entropy, which is ample for a single-host process. Encoded
// as unpadded base32 (5 bits per character) this produces 13 characters
// of suffix.
const idRandomBytes = 8

// NewID returns a fresh sandbox identifier of the form `sbx_xxx...`.
func NewID() (string, error) {
	return newIDWithPrefix(sandboxIDPrefix)
}

// NewSnapshotID returns a fresh snapshot identifier of the form `snap_xxx...`.
func NewSnapshotID() (string, error) {
	return newIDWithPrefix(snapshotIDPrefix)
}

func newIDWithPrefix(prefix string) (string, error) {
	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("sandbox: generate id: %w", err)
	}
	enc := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:])
	return prefix + strings.ToLower(enc), nil
}

// IsValidID reports whether s has the shape of a sandbox ID.
func IsValidID(s string) bool { return hasValidShape(s, sandboxIDPrefix) }

// IsValidSnapshotID reports whether s has the shape of a snapshot ID.
func IsValidSnapshotID(s string) bool { return hasValidShape(s, snapshotIDPrefix) }

func hasValidShape(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	suffix := strings.ToUpper(strings.TrimPrefix(s, prefix))
	if suffix == "" {
		return false
	}
	_, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(suffix)
	return err == nil
}
