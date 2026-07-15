package cryptdev

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// KEKSize is the master key-encryption-key length (AES-256). It matches
// secretstore.KeySize so the same daemon master key can serve both.
const KEKSize = 32

// DEKSize is the length of a per-volume data-encryption key — the LUKS
// passphrase. 32 random bytes = 256 bits of entropy unlocking the keyslot.
const DEKSize = 32

// NewDEK returns a fresh random per-volume key.
func NewDEK() ([]byte, error) {
	k := make([]byte, DEKSize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("cryptdev: generate dek: %w", err)
	}
	return k, nil
}

// WrapKey seals dek under the master KEK with AES-256-GCM, binding it to aad
// (the volume name) so a wrapped key can't be transplanted to another volume.
// The result is nonce||ciphertext, safe to persist in the volume record — it is
// inert without the KEK, which lives outside the store (a key file / env / KMS).
func WrapKey(kek, dek, aad []byte) ([]byte, error) {
	aead, err := gcm(kek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("cryptdev: nonce: %w", err)
	}
	// Seal prepends nonce to the ciphertext (dst == nonce), so the result is
	// nonce||ct — exactly what UnwrapKey splits back apart.
	return aead.Seal(nonce, nonce, dek, aad), nil
}

// UnwrapKey reverses WrapKey. It fails if the KEK is wrong, the aad (volume name)
// differs, or the wrapped bytes were tampered with — GCM's tag catches all three.
func UnwrapKey(kek, wrapped, aad []byte) ([]byte, error) {
	aead, err := gcm(kek)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(wrapped) < ns {
		return nil, fmt.Errorf("cryptdev: wrapped key too short (%d bytes)", len(wrapped))
	}
	nonce, ct := wrapped[:ns], wrapped[ns:]
	dek, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("cryptdev: unwrap: %w", err)
	}
	return dek, nil
}

func gcm(kek []byte) (cipher.AEAD, error) {
	if len(kek) != KEKSize {
		return nil, fmt.Errorf("cryptdev: KEK must be %d bytes, got %d", KEKSize, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("cryptdev: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptdev: gcm: %w", err)
	}
	return aead, nil
}
