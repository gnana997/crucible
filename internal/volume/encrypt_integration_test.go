//go:build linux

package volume

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func requireCryptRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (cryptsetup + mount)")
	}
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("cryptsetup not installed")
	}
}

// TestEncryptedVolumeRoundTrip is the end-to-end proof for encrypted volumes:
// with a real cryptsetup it
// formats an encrypted volume, writes a marker through the decrypted device,
// proves the on-disk container is ciphertext (the marker is absent), proves the
// data survives close/reopen, and proves Shred renders it permanently
// unrecoverable. Root-gated (LUKS + mount need CAP_SYS_ADMIN); skips otherwise.
func TestEncryptedVolumeRoundTrip(t *testing.T) {
	requireCryptRoot(t)

	dir := t.TempDir()
	m := newMgr(t, dir)
	if err := m.EnableEncryption(keyring(kek32(t)), "k1", false); err != nil {
		t.Fatalf("EnableEncryption: %v", err)
	}

	const size = 64 << 20 // 64 MiB usable — room for the LUKS header + a small ext4
	marker := []byte("MARKER-crypto-shred-proof-9f3a2c7e")

	yes := true
	rec, err := m.Create("data", size, CreateOpts{Encrypt: &yes})
	if err != nil {
		t.Fatalf("Create(encrypt): %v", err)
	}
	if !rec.Encrypted || len(rec.WrappedKey) == 0 {
		t.Fatalf("record not marked encrypted / no wrapped key: %+v", rec)
	}
	img := filepath.Join(dir, "data.img")

	// The raw container must be a LUKS header, never plaintext ext4.
	if !looksLikeLUKS(img) {
		t.Fatal("backing file is not a LUKS container")
	}

	// --- write the marker through the decrypted device ---
	writeMarker := func() {
		mapper, err := m.OpenDevice("data")
		if err != nil {
			t.Fatalf("OpenDevice: %v", err)
		}
		defer func() {
			if err := m.CloseDevice("data"); err != nil {
				t.Errorf("CloseDevice: %v", err)
			}
		}()
		mnt := t.TempDir()
		if err := syscall.Mount(mapper, mnt, "ext4", 0, ""); err != nil {
			t.Fatalf("mount %s: %v", mapper, err)
		}
		defer func() { _ = syscall.Unmount(mnt, 0) }()
		if err := os.WriteFile(filepath.Join(mnt, "secret.txt"), marker, 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		_ = syscall.Unmount(mnt, 0)
	}
	writeMarker()

	// --- the on-disk container must NOT contain the plaintext marker ---
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, marker) {
		t.Fatal("SECURITY: the on-disk container contains the plaintext marker (not encrypted)")
	}

	// --- data survives close + reopen ---
	readMarker := func() []byte {
		mapper, err := m.OpenDevice("data")
		if err != nil {
			t.Fatalf("reopen OpenDevice: %v", err)
		}
		defer func() { _ = m.CloseDevice("data") }()
		mnt := t.TempDir()
		if err := syscall.Mount(mapper, mnt, "ext4", 0, ""); err != nil {
			t.Fatalf("remount: %v", err)
		}
		defer func() { _ = syscall.Unmount(mnt, 0) }()
		got, err := os.ReadFile(filepath.Join(mnt, "secret.txt"))
		if err != nil {
			t.Fatalf("read marker after reopen: %v", err)
		}
		return got
	}
	if got := readMarker(); !bytes.Equal(got, marker) {
		t.Fatalf("marker after reopen = %q, want %q", got, marker)
	}

	// --- Shred: the data is now permanently unrecoverable ---
	if err := m.Shred("data"); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if _, err := m.OpenDevice("data"); err == nil {
		t.Fatal("OpenDevice succeeded after Shred — data not crypto-shredded")
	}
	if _, statErr := os.Stat(img); !os.IsNotExist(statErr) {
		t.Fatalf("backing file still present after Shred: %v", statErr)
	}
	if _, err := m.Get("data"); err != ErrNotFound {
		t.Fatalf("record present after Shred: %v", err)
	}
}

// TestAttachOpensAndReleaseClosesDevice proves the boot-path lifecycle: Attach
// of an encrypted volume returns its decrypted /dev/mapper block device (so the
// jailer can mknod it into the chroot), and Release closes it — no leaked mapper.
func TestAttachOpensAndReleaseClosesDevice(t *testing.T) {
	requireCryptRoot(t)

	m := newMgr(t, t.TempDir())
	if err := m.EnableEncryption(keyring(kek32(t)), "k1", false); err != nil {
		t.Fatalf("EnableEncryption: %v", err)
	}
	yes := true
	if _, err := m.Create("data", 64<<20, CreateOpts{Encrypt: &yes}); err != nil {
		t.Fatalf("Create(encrypt): %v", err)
	}

	path, encrypted, err := m.Attach("data", "sbx1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !encrypted {
		t.Fatal("Attach must report an encrypted volume as encrypted")
	}
	if path != "/dev/mapper/crucible-vol-data" {
		t.Fatalf("Attach path = %q, want the mapper node", path)
	}
	// The returned path must be a live block device.
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat mapper: %v", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		t.Fatalf("mapper is not a block device: mode %#o", st.Mode)
	}

	// Release closes the device — the mapper node must be gone afterward.
	m.Release("data")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mapper still present after Release (leaked device): %v", err)
	}

	// A plaintext volume, by contrast, attaches to its backing file, not a device.
	if _, err := m.Create("plain", 8<<20, CreateOpts{}); err != nil {
		t.Fatalf("Create plaintext: %v", err)
	}
	p2, enc2, err := m.Attach("plain", "sbx2")
	if err != nil {
		t.Fatalf("Attach plaintext: %v", err)
	}
	if enc2 || filepath.Base(p2) != "plain.img" {
		t.Fatalf("plaintext Attach = (%q, encrypted=%v), want the .img file", p2, enc2)
	}
	m.Release("plain")
}

// TestKeyringMultiKeyAndRetire proves the keyring: volumes wrap under different
// keys (default + a per-create override), each opens under its own key, and
// retiring a key makes only the volumes that used it unopenable — with a clear
// ErrKeyNotFound, not a silent failure.
func TestKeyringMultiKeyAndRetire(t *testing.T) {
	requireCryptRoot(t)

	dir := t.TempDir()
	k1, k2 := kek32(t), kek32(t)
	m := newMgr(t, dir)
	if err := m.EnableEncryption(map[string][]byte{"k1": k1, "k2": k2}, "k1", false); err != nil {
		t.Fatalf("EnableEncryption: %v", err)
	}
	yes := true
	ra, err := m.Create("a", 64<<20, CreateOpts{Encrypt: &yes}) // default key (k1)
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if ra.KeyID != "k1" {
		t.Fatalf("volume a KeyID = %q, want k1", ra.KeyID)
	}
	rb, err := m.Create("b", 64<<20, CreateOpts{Encrypt: &yes, KeyID: "k2"}) // explicit key
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if rb.KeyID != "k2" {
		t.Fatalf("volume b KeyID = %q, want k2", rb.KeyID)
	}
	for _, v := range []string{"a", "b"} {
		if _, err := m.OpenDevice(v); err != nil {
			t.Fatalf("open %s: %v", v, err)
		}
		if err := m.CloseDevice(v); err != nil {
			t.Fatalf("close %s: %v", v, err)
		}
	}
	_ = m.Close()

	// Reopen the store with k2 RETIRED (only k1 in the ring).
	m2, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()
	if err := m2.EnableEncryption(map[string][]byte{"k1": k1}, "k1", false); err != nil {
		t.Fatalf("EnableEncryption (retired k2): %v", err)
	}
	if _, err := m2.OpenDevice("a"); err != nil {
		t.Fatalf("a should still open under k1: %v", err)
	}
	_ = m2.CloseDevice("a")
	if _, err := m2.OpenDevice("b"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("b under a retired key = %v, want ErrKeyNotFound", err)
	}
}

// TestRewrapKeepsDataThenRetireOldKey proves rotation on a real LUKS container:
// a volume created under k1 is rewrapped to k2 (touching only the record), and
// after the old key k1 is retired it still opens under k2 with its data intact —
// the container itself was never re-encrypted.
func TestRewrapKeepsDataThenRetireOldKey(t *testing.T) {
	requireCryptRoot(t)

	dir := t.TempDir()
	k1, k2 := kek32(t), kek32(t)
	m := newMgr(t, dir)
	if err := m.EnableEncryption(map[string][]byte{"k1": k1, "k2": k2}, "k1", false); err != nil {
		t.Fatalf("EnableEncryption: %v", err)
	}
	yes := true
	if _, err := m.Create("data", 64<<20, CreateOpts{Encrypt: &yes}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	marker := []byte("REWRAP-marker-7c1e")
	writeVolMarker(t, m, "data", marker)

	// Rotate the key: k1 → k2. No cryptsetup on the container, no data movement.
	if err := m.Rewrap("data", "k2"); err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if got, _ := m.Get("data"); got.KeyID != "k2" {
		t.Fatalf("KeyID after rewrap = %q, want k2", got.KeyID)
	}
	_ = m.Close()

	// Retire k1 entirely: reopen with only k2. The volume must still open + read.
	m2, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()
	if err := m2.EnableEncryption(map[string][]byte{"k2": k2}, "k2", false); err != nil {
		t.Fatalf("EnableEncryption (only k2): %v", err)
	}
	if got := readVolMarker(t, m2, "data"); !bytes.Equal(got, marker) {
		t.Fatalf("marker after rewrap+retire = %q, want %q", got, marker)
	}
}

// writeVolMarker mounts an encrypted volume's device and writes a marker file.
func writeVolMarker(t *testing.T, m *Manager, name string, marker []byte) {
	t.Helper()
	mapper, err := m.OpenDevice(name)
	if err != nil {
		t.Fatalf("OpenDevice %s: %v", name, err)
	}
	defer func() { _ = m.CloseDevice(name) }()
	mnt := t.TempDir()
	if err := syscall.Mount(mapper, mnt, "ext4", 0, ""); err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer func() { _ = syscall.Unmount(mnt, 0) }()
	if err := os.WriteFile(filepath.Join(mnt, "m.txt"), marker, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// readVolMarker mounts an encrypted volume's device and reads the marker file.
func readVolMarker(t *testing.T, m *Manager, name string) []byte {
	t.Helper()
	mapper, err := m.OpenDevice(name)
	if err != nil {
		t.Fatalf("OpenDevice %s: %v", name, err)
	}
	defer func() { _ = m.CloseDevice(name) }()
	mnt := t.TempDir()
	if err := syscall.Mount(mapper, mnt, "ext4", 0, ""); err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer func() { _ = syscall.Unmount(mnt, 0) }()
	got, err := os.ReadFile(filepath.Join(mnt, "m.txt"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return got
}
