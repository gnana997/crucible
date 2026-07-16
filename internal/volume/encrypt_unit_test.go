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

// setKeyring wires a keyring directly, bypassing EnableEncryption's cryptsetup
// probe — so the pure-crypto rewrap/reload logic is testable without root. The
// engine is set non-nil but never invoked (rewrap touches only the record).
func setKeyring(m *Manager, keys map[string][]byte, defaultKeyID string) {
	m.crypt = cryptdev.New()
	m.keys = keys
	m.defaultKeyID = defaultKeyID
}

func TestRewrapReKeysWithoutTouchingData(t *testing.T) {
	m := newMgr(t, t.TempDir())
	k1, k2 := kek32(t), kek32(t)
	setKeyring(m, map[string][]byte{"k1": k1, "k2": k2}, "k1")

	dek, _ := cryptdev.NewDEK()
	wrapped, _ := cryptdev.WrapKey(k1, dek, []byte("data"))
	if err := m.st.put(Record{Name: "data", SizeBytes: testSize, Formatted: true, HostID: "h", Encrypted: true, WrappedKey: wrapped, KeyID: "k1"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Rewrap("data", "k2"); err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	got, err := m.Get("data")
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != "k2" {
		t.Fatalf("KeyID = %q, want k2", got.KeyID)
	}
	// The data key must be unchanged — recovered under k2, identical to the original.
	back, err := cryptdev.UnwrapKey(k2, got.WrappedKey, []byte("data"))
	if err != nil {
		t.Fatalf("unwrap under k2: %v", err)
	}
	if string(back) != string(dek) {
		t.Fatal("the data key changed on rewrap — the volume would be unopenable")
	}
	// It must no longer open under the old key.
	if _, err := cryptdev.UnwrapKey(k1, got.WrappedKey, []byte("data")); err == nil {
		t.Fatal("the rewrapped key still opens under the old KEK")
	}

	// Rewrap to an unknown key id is refused.
	if err := m.Rewrap("data", "ghost"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Rewrap to unknown key = %v, want ErrKeyNotFound", err)
	}
}

func TestReloadKeyringRefusesDroppingUsedKey(t *testing.T) {
	m := newMgr(t, t.TempDir())
	k1, k2 := kek32(t), kek32(t)
	setKeyring(m, map[string][]byte{"k1": k1, "k2": k2}, "k1")
	if err := m.st.put(Record{Name: "data", SizeBytes: testSize, Formatted: true, HostID: "h", Encrypted: true, WrappedKey: []byte{1, 2, 3, 4}, KeyID: "k2"}); err != nil {
		t.Fatal(err)
	}
	// Dropping k2 while a volume uses it must be refused.
	if err := m.ReloadKeyring(map[string][]byte{"k1": k1}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("reload dropping an in-use key = %v, want ErrKeyNotFound", err)
	}
	// Keeping both (and adding a third) is fine.
	if err := m.ReloadKeyring(map[string][]byte{"k1": k1, "k2": k2, "k3": kek32(t)}); err != nil {
		t.Fatalf("reload keeping in-use keys: %v", err)
	}
	if _, ok := m.kekFor("k3"); !ok {
		t.Fatal("reloaded keyring missing the new key k3")
	}
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
