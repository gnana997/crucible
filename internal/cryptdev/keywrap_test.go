package cryptdev

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KEKSize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	kek := testKEK(t)
	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapKey(kek, dek, []byte("data"))
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("SECURITY: wrapped blob contains the plaintext DEK")
	}
	got, err := UnwrapKey(kek, wrapped, []byte("data"))
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("round-trip mismatch")
	}
}

func TestUnwrapWrongKEKFails(t *testing.T) {
	dek, _ := NewDEK()
	wrapped, _ := WrapKey(testKEK(t), dek, []byte("data"))
	if _, err := UnwrapKey(testKEK(t), wrapped, []byte("data")); err == nil {
		t.Fatal("unwrap with a different KEK must fail")
	}
}

func TestUnwrapWrongNameFails(t *testing.T) {
	kek := testKEK(t)
	dek, _ := NewDEK()
	wrapped, _ := WrapKey(kek, dek, []byte("data"))
	// A wrapped key transplanted to another volume name must not open — this is
	// what stops moving one volume's key onto another's ciphertext.
	if _, err := UnwrapKey(kek, wrapped, []byte("other")); err == nil {
		t.Fatal("unwrap with a different volume name (AAD) must fail")
	}
}

func TestUnwrapTamperFails(t *testing.T) {
	kek := testKEK(t)
	dek, _ := NewDEK()
	wrapped, _ := WrapKey(kek, dek, []byte("data"))
	wrapped[len(wrapped)-1] ^= 0xff // flip a ciphertext byte
	if _, err := UnwrapKey(kek, wrapped, []byte("data")); err == nil {
		t.Fatal("unwrap of tampered ciphertext must fail (GCM tag)")
	}
}

func TestWrapRejectsBadKEK(t *testing.T) {
	if _, err := WrapKey(make([]byte, 16), make([]byte, 32), nil); err == nil {
		t.Fatal("WrapKey must reject a KEK that is not 32 bytes")
	}
}
