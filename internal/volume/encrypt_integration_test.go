//go:build linux

package volume

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestEncryptedVolumeRoundTrip is M1's exit bar: with a real cryptsetup it
// formats an encrypted volume, writes a marker through the decrypted device,
// proves the on-disk container is ciphertext (the marker is absent), proves the
// data survives close/reopen, and proves Shred renders it permanently
// unrecoverable. Root-gated (LUKS + mount need CAP_SYS_ADMIN); skips otherwise.
func TestEncryptedVolumeRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (cryptsetup + mount)")
	}
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("cryptsetup not installed")
	}

	dir := t.TempDir()
	m := newMgr(t, dir)
	if err := m.EnableEncryption(kek32(t), "k1", false); err != nil {
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
