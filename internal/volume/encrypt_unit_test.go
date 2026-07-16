package volume

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/internal/cryptdev"
)

func kek32(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, cryptdev.KEKSize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// keyring wraps a single key as a one-entry keyring under id "k1".
func keyring(kek []byte) map[string][]byte { return map[string][]byte{"k1": kek} }

// These tests exercise the Manager's encryption surface WITHOUT root/cryptsetup:
// the disabled-path guards, error contracts, record persistence, and the LUKS
// backfill guard. The real format→open→write→read round trip is in the
// root-gated integration test.

func TestCreateEncryptedWithoutKeyDisabled(t *testing.T) {
	m := newMgr(t, t.TempDir())
	yes := true
	if _, err := m.Create("data", testSize, CreateOpts{Encrypt: &yes}); !errors.Is(err, ErrEncryptionDisabled) {
		t.Fatalf("Create(encrypt) with no key = %v, want ErrEncryptionDisabled", err)
	}
	if m.EncryptionEnabled() {
		t.Fatal("EncryptionEnabled should be false before EnableEncryption")
	}
}

func TestEnableEncryptionRejectsBadKey(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if err := m.EnableEncryption(keyring(make([]byte, 16)), "k1", false); err == nil {
		t.Fatal("EnableEncryption must reject a 16-byte key")
	}
}

// The keyring validations run before the cryptsetup probe, so they're testable
// without root/cryptsetup.
func TestEnableEncryptionKeyringValidation(t *testing.T) {
	m := newMgr(t, t.TempDir())
	good := kek32(t)
	if err := m.EnableEncryption(map[string][]byte{}, "default", false); err == nil {
		t.Fatal("an empty keyring must error")
	}
	if err := m.EnableEncryption(map[string][]byte{"k1": good}, "default", false); err == nil {
		t.Fatal("a default key id absent from the keyring must error")
	}
	if err := m.EnableEncryption(map[string][]byte{"k1": good, "k2": make([]byte, 16)}, "k1", false); err == nil {
		t.Fatal("a wrong-size key anywhere in the keyring must error")
	}
}

func TestShredRejectsPlaintextVolume(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Shred("data"); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("Shred(plaintext) = %v, want ErrNotEncrypted", err)
	}
	// The plaintext volume must still be intact.
	if _, err := m.Get("data"); err != nil {
		t.Fatalf("plaintext volume gone after refused Shred: %v", err)
	}
}

func TestOpenDeviceContracts(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if _, err := m.OpenDevice("nope"); !errors.Is(err, ErrEncryptionDisabled) {
		t.Fatalf("OpenDevice with no key = %v, want ErrEncryptionDisabled", err)
	}
	// With encryption enabled but a plaintext volume, OpenDevice rejects.
	if err := m.EnableEncryption(keyring(kek32(t)), "k1", false); err != nil {
		t.Skipf("EnableEncryption unavailable here (cryptsetup missing?): %v", err)
	}
	if _, err := m.Create("plain", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create plaintext: %v", err)
	}
	if _, err := m.OpenDevice("plain"); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("OpenDevice(plaintext) = %v, want ErrNotEncrypted", err)
	}
	if _, err := m.OpenDevice("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenDevice(absent) = %v, want ErrNotFound", err)
	}
}

func TestEncryptedRecordRoundTrips(t *testing.T) {
	// A record carrying the encryption fields must persist + reload intact, and a
	// plaintext record must serialize without the new keys (omitempty).
	enc := Record{
		Name: "data", SizeBytes: testSize, Formatted: true, HostID: "h",
		Encrypted: true, WrappedKey: []byte{1, 2, 3, 4}, KeyID: "k1",
	}
	b, err := json.Marshal(enc)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Encrypted || back.KeyID != "k1" || string(back.WrappedKey) != string(enc.WrappedKey) {
		t.Fatalf("encrypted record did not round-trip: %+v", back)
	}
	plain := Record{Name: "p", SizeBytes: testSize, Formatted: true, HostID: "h"}
	pb, _ := json.Marshal(plain)
	for _, k := range []string{"encrypted", "wrapped_key", "key_id"} {
		if containsKey(pb, k) {
			t.Errorf("plaintext record leaked %q into JSON: %s", k, pb)
		}
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	_, ok := m[key]
	return ok
}

func TestBackfillSkipsLUKSContainer(t *testing.T) {
	dir := t.TempDir()
	// Drop a file that begins with the LUKS magic but has no record — simulating
	// a record-less encrypted container. Backfill must NOT mistype it as plaintext.
	luks := append([]byte{'L', 'U', 'K', 'S', 0xba, 0xbe}, make([]byte, 128)...)
	if err := os.WriteFile(filepath.Join(dir, "orphan.img"), luks, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newMgr(t, dir) // NewManager runs backfill
	if _, err := m.Get("orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backfill mistyped a LUKS container as a volume: %v", err)
	}
}
