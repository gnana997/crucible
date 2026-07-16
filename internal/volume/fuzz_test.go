package volume

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzApplyDelta feeds arbitrary bytes as a .delta onto a small target image.
// An imported delta is attacker-influenced (gated by the volume_backup op), so
// parsing it must never panic or exhaust memory — it may only apply valid blocks
// or return an error.
func FuzzApplyDelta(f *testing.F) {
	// Seed with a real delta (block 1 of a 3-block image changed).
	dir := f.TempDir()
	const bs = 1024
	base := filepath.Join(dir, "seed_base.img")
	cur := filepath.Join(dir, "seed_cur.img")
	if err := os.WriteFile(base, make([]byte, bs*3), 0o600); err != nil {
		f.Fatal(err)
	}
	data := make([]byte, bs*3)
	for i := bs; i < bs*2; i++ {
		data[i] = 0xAB
	}
	if err := os.WriteFile(cur, data, 0o600); err != nil {
		f.Fatal(err)
	}
	pm, err := computeManifest(base, bs)
	if err != nil {
		f.Fatal(err)
	}
	seedDelta := filepath.Join(dir, "seed.delta")
	if _, err := writeDelta(cur, pm, seedDelta); err != nil {
		f.Fatal(err)
	}
	if b, err := os.ReadFile(seedDelta); err == nil {
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte("CRUCDLT1"))
	// A header claiming a 4 GiB block size + huge counts — must be rejected, not OOM.
	f.Add([]byte("CRUCDLT1\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))

	f.Fuzz(func(t *testing.T, delta []byte) {
		dp := filepath.Join(t.TempDir(), "f.delta")
		if err := os.WriteFile(dp, delta, 0o600); err != nil {
			t.Skip()
		}
		target := filepath.Join(t.TempDir(), "t.img")
		if err := os.WriteFile(target, make([]byte, bs*3), 0o600); err != nil {
			t.Skip()
		}
		// Must return (nil or error) without panicking. We don't assert which.
		_ = applyDelta(target, dp)
	})
}

// FuzzReadManifest feeds arbitrary bytes as a manifest sidecar. A corrupt or
// hostile header must not panic (a zero block size once divided) or over-allocate.
func FuzzReadManifest(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("CRUCMAN1"))
	// magic + blockSize=0 (would divide-by-zero) + huge image + huge count.
	f.Add([]byte("CRUCMAN1\x00\x00\x00\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, man []byte) {
		mp := filepath.Join(t.TempDir(), "f.manifest")
		if err := os.WriteFile(mp, man, 0o600); err != nil {
			t.Skip()
		}
		_, _ = readManifest(mp)
	})
}
