package sandbox

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// idPrefix is the literal prefix every sandbox ID starts with. Makes IDs
// self-identifying in logs and URLs — `sbx_...` is easy to spot compared
// to a bare hex string.
const idPrefix = "sbx_"

// idRandomBytes is the number of random bytes hashed into each ID. 8 bytes
// = 64 bits of entropy, which is ample for a single-host process. Encoded
// as unpadded base32 (5 bits per character) this produces 13 characters
// of suffix, for a total ID length of 17 (4-byte prefix + 13).
const idRandomBytes = 8

// NewID returns a fresh, URL-safe sandbox identifier of the form `sbx_xxx...`.
// The random portion uses crypto/rand and base32 (hex alphabet, lowercased,
// no padding) so IDs are safe in paths, log lines, and shell arguments.
func NewID() (string, error) {
	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("sandbox: generate id: %w", err)
	}
	enc := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:])
	return idPrefix + strings.ToLower(enc), nil
}

// IsValidID reports whether s has the shape of a sandbox ID. It checks the
// prefix and the encoded suffix character set. Does not verify the suffix
// came from NewID — only that it could have.
func IsValidID(s string) bool {
	if !strings.HasPrefix(s, idPrefix) {
		return false
	}
	suffix := strings.ToUpper(strings.TrimPrefix(s, idPrefix))
	if suffix == "" {
		return false
	}
	_, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(suffix)
	return err == nil
}
