package secretstore

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvKeyVar holds a base64-encoded master key. It takes precedence over a key
// file so a KMS / systemd-credential wrapper (or the control plane) can inject
// the key without it ever touching the same disk as the ciphertext.
const EnvKeyVar = "CRUCIBLE_SECRETS_KEY"

// LoadMasterKey resolves the AES-256 secret-store master key from
// CRUCIBLE_SECRETS_KEY or keyFile — see LoadMasterKeyFrom.
func LoadMasterKey(keyFile string) (key []byte, generated bool, err error) {
	return LoadMasterKeyFrom(EnvKeyVar, keyFile)
}

// LoadMasterKeyFrom resolves an AES-256 master key, used both for the secret
// store and (with a different envVar) for per-volume encryption:
//
//   - $envVar (base64) wins if set.
//   - else keyFile (base64). If keyFile is set but ABSENT, a fresh key is
//     generated and written 0600 (first-run convenience) and generated=true —
//     the caller should log a "back this key up" warning.
//   - if NEITHER is configured, returns (nil, false, nil): the feature is
//     disabled. There is no silent fallback — opting in is explicit.
func LoadMasterKeyFrom(envVar, keyFile string) (key []byte, generated bool, err error) {
	if v := os.Getenv(envVar); v != "" {
		k, derr := decodeKey(v)
		if derr != nil {
			return nil, false, fmt.Errorf("secretstore: %s: %w", envVar, derr)
		}
		return k, false, nil
	}
	if keyFile == "" {
		return nil, false, nil // disabled
	}
	b, rerr := os.ReadFile(keyFile)
	if errors.Is(rerr, os.ErrNotExist) {
		k, gerr := GenerateKey()
		if gerr != nil {
			return nil, false, gerr
		}
		if werr := writeKeyFile(keyFile, k); werr != nil {
			return nil, false, fmt.Errorf("secretstore: write new key %s: %w", keyFile, werr)
		}
		return k, true, nil
	}
	if rerr != nil {
		return nil, false, fmt.Errorf("secretstore: read %s: %w", keyFile, rerr)
	}
	k, derr := decodeKey(string(b))
	if derr != nil {
		return nil, false, fmt.Errorf("secretstore: %s: %w", keyFile, derr)
	}
	return k, false, nil
}

// GenerateKey returns a fresh random 32-byte key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("secretstore: generate key: %w", err)
	}
	return k, nil
}

func decodeKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(k) != KeySize {
		return nil, fmt.Errorf("key must decode to %d bytes, got %d", KeySize, len(k))
	}
	return k, nil
}

func writeKeyFile(path string, key []byte) error {
	enc := base64.StdEncoding.EncodeToString(key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(enc+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
