package volume

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeImg writes size bytes to a fresh file (deterministic content seeded by b).
func writeImg(t *testing.T, path string, size int, b byte) {
	t.Helper()
	data := bytes.Repeat([]byte{b}, size)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDeltaRoundTripAndVerify(t *testing.T) {
	dir := t.TempDir()
	// A 5-block image (block size 1 KiB for the test) + a couple changed blocks.
	const bs = 1024
	base := filepath.Join(dir, "base.img")
	cur := filepath.Join(dir, "cur.img")
	writeImg(t, base, bs*5, 0xAA)

	// current = base with block 1 and block 3 changed.
	data := bytes.Repeat([]byte{0xAA}, bs*5)
	copy(data[bs*1:bs*2], bytes.Repeat([]byte{0xBB}, bs))
	copy(data[bs*3:bs*4], bytes.Repeat([]byte{0xCC}, bs))
	if err := os.WriteFile(cur, data, 0o600); err != nil {
		t.Fatal(err)
	}

	parentMan, err := computeManifest(base, bs)
	if err != nil {
		t.Fatalf("computeManifest: %v", err)
	}
	delta := filepath.Join(dir, "d.delta")
	curMan, err := writeDelta(cur, parentMan, delta)
	if err != nil {
		t.Fatalf("writeDelta: %v", err)
	}
	// Only 2 blocks changed, so the delta is far smaller than the full image.
	di, _ := os.Stat(delta)
	if di.Size() >= int64(bs*5) {
		t.Fatalf("delta size %d not smaller than full %d", di.Size(), bs*5)
	}

	// Apply onto a copy of base → must equal cur, and verify against curMan.
	recon := filepath.Join(dir, "recon.img")
	writeImg(t, recon, bs*5, 0xAA)
	if err := applyDelta(recon, delta); err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	got, _ := os.ReadFile(recon)
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed image != current image")
	}
	if err := verifyImage(recon, curMan); err != nil {
		t.Fatalf("verifyImage: %v", err)
	}
}

func TestDeltaGrowth(t *testing.T) {
	dir := t.TempDir()
	const bs = 1024
	base := filepath.Join(dir, "base.img")
	cur := filepath.Join(dir, "cur.img")
	writeImg(t, base, bs*2, 0x11)
	// current grew to 4 blocks (blocks 2,3 are new) and changed block 0.
	data := bytes.Repeat([]byte{0x11}, bs*4)
	copy(data[0:bs], bytes.Repeat([]byte{0x22}, bs))
	copy(data[bs*2:], bytes.Repeat([]byte{0x33}, bs*2))
	if err := os.WriteFile(cur, data, 0o600); err != nil {
		t.Fatal(err)
	}
	parentMan, _ := computeManifest(base, bs)
	delta := filepath.Join(dir, "d.delta")
	if _, err := writeDelta(cur, parentMan, delta); err != nil {
		t.Fatalf("writeDelta: %v", err)
	}
	recon := filepath.Join(dir, "recon.img")
	writeImg(t, recon, bs*2, 0x11)
	if err := applyDelta(recon, delta); err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	got, _ := os.ReadFile(recon)
	if !bytes.Equal(got, data) {
		t.Fatalf("grown reconstruction mismatch (got %d bytes, want %d)", len(got), len(data))
	}
}

func TestManifestSerialization(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.img")
	writeImg(t, img, 4096+123, 0x7E) // non-block-aligned tail
	man, err := computeManifest(img, 1024)
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "x.manifest")
	if err := writeManifest(mp, man); err != nil {
		t.Fatal(err)
	}
	got, err := readManifest(mp)
	if err != nil {
		t.Fatal(err)
	}
	if got.BlockSize != man.BlockSize || got.ImageSize != man.ImageSize || len(got.Hashes) != len(man.Hashes) {
		t.Fatalf("manifest roundtrip header mismatch")
	}
	for i := range man.Hashes {
		if got.Hashes[i] != man.Hashes[i] {
			t.Fatalf("hash %d mismatch after roundtrip", i)
		}
	}
}

// Full Manager-level chain: full backup -> mutate -> incremental -> restore tip.
func TestIncrementalChainRestore(t *testing.T) {
	needResize2fs(t) // uses mkfs via newMgr
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Write a marker into the (raw) image so we can prove the data survives the
	// chain. We touch the backing file directly at a fixed offset — enough to
	// make a block differ between the full and the incremental.
	imgPath := filepath.Join(m.dir, "data.img")
	poke := func(off int64, b byte) {
		f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteAt(bytes.Repeat([]byte{b}, 4096), off); err != nil {
			t.Fatal(err)
		}
	}
	poke(0, 0x01)

	full, err := m.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if full.Kind != backupKindFull {
		t.Fatalf("full.Kind = %q", full.Kind)
	}

	// Mutate a different region, then take an incremental against the full.
	poke(int64(testSize)/2, 0x02)
	inc, err := m.BackupIncremental("data", full.ID)
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}
	if inc.Kind != backupKindIncremental || inc.ParentID != full.ID {
		t.Fatalf("inc record wrong: kind=%q parent=%q", inc.Kind, inc.ParentID)
	}
	// The delta must be far smaller than a full copy.
	di, _ := os.Stat(inc.Path)
	if di.Size() >= testSize/2 {
		t.Fatalf("delta %d not much smaller than image %d", di.Size(), testSize)
	}

	// Snapshot the exact current image bytes, then restore the incremental tip
	// into a new volume and compare its backing file byte-for-byte.
	want, _ := os.ReadFile(imgPath)
	if _, err := m.RestoreTo(inc.ID, "restored"); err != nil {
		t.Fatalf("RestoreTo(incremental): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.dir, "restored.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("restored image != source image at the incremental's point in time")
	}
}

func TestDeleteBackupRefusesParent(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatal(err)
	}
	full, err := m.Backup("data")
	if err != nil {
		t.Fatal(err)
	}
	inc, err := m.BackupIncremental("data", full.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Deleting the parent full is refused while the incremental depends on it.
	if err := m.DeleteBackup(full.ID); !errors.Is(err, ErrBackupHasChildren) {
		t.Fatalf("DeleteBackup(parent) = %v, want ErrBackupHasChildren", err)
	}
	// Deleting the child first, then the parent, works — and drops the sidecar.
	if err := m.DeleteBackup(inc.ID); err != nil {
		t.Fatalf("DeleteBackup(child): %v", err)
	}
	if _, err := os.Stat(backupManifestPath(inc.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incremental manifest sidecar not removed")
	}
	if err := m.DeleteBackup(full.ID); err != nil {
		t.Fatalf("DeleteBackup(parent after child): %v", err)
	}
}

func TestParentVolumeMismatch(t *testing.T) {
	needResize2fs(t)
	m := newMgr(t, t.TempDir())
	if _, err := m.Create("a", testSize, CreateOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("b", testSize, CreateOpts{}); err != nil {
		t.Fatal(err)
	}
	full, err := m.Backup("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BackupIncremental("b", full.ID); !errors.Is(err, ErrParentVolumeMismatch) {
		t.Fatalf("cross-volume parent = %v, want ErrParentVolumeMismatch", err)
	}
}
