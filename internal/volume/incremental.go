package volume

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnana997/crucible/internal/fsutil"
)

// Incremental backups. A backup is either a "full" (a whole-image copy — the
// v0.6.3 behaviour, unchanged) or an "incremental": only the fixed-size blocks
// that changed since a parent backup, plus a manifest of per-block hashes.
//
// The mechanism is block-level over the backup's backing FILE (the ext4 image,
// or the LUKS ciphertext container for an encrypted volume — hashing ciphertext
// works because AES-XTS is per-block, so encryption is transparent here). There
// is no changed-block tracking in the stack, so a delta is computed by reading
// the current image and comparing each block's hash to the parent's manifest;
// only differing blocks are written. Restore walks the chain from its base full,
// applies each delta in order, and (when the tip's manifest is present) verifies
// the reconstructed image block-for-block.

const (
	// defaultBackupBlockSize is the fixed block granularity for delta + manifest.
	// 1 MiB keeps a manifest small (a 50 GiB image → ~1.6 MiB of hashes) while
	// still capturing changes at a useful resolution.
	defaultBackupBlockSize = 1 << 20

	// maxBackupBlockSize bounds a block size parsed from an (untrusted) imported
	// delta or an on-disk manifest, so a hostile/corrupt header can't drive a
	// multi-gigabyte allocation. Our own deltas use defaultBackupBlockSize.
	maxBackupBlockSize = 64 << 20

	// maxManifestBlocks caps the hash count a manifest header may claim, so a
	// corrupt count can't drive an unbounded slice allocation. 16M blocks covers a
	// 16 TiB volume at the 1 MiB default — far beyond any realistic volume.
	maxManifestBlocks = 1 << 24

	backupKindFull        = "full"
	backupKindIncremental = "incremental"

	manifestMagic = "CRUCMAN1"
	deltaMagic    = "CRUCDLT1"
	deltaHeader   = 8 + 4 + 8 + 8 // magic + blockSize(u32) + imageSize(u64) + nChanged(u64)
)

var (
	// ErrBackupChainBroken means an incremental's chain to its base full is
	// missing a link, cyclic, or the reconstructed image failed verification.
	ErrBackupChainBroken = errors.New("volume: incremental backup chain is broken (a link is missing or corrupt)")
	// ErrBackupHasChildren means a backup can't be deleted because an incremental
	// still depends on it as a parent.
	ErrBackupHasChildren = errors.New("volume: backup has dependent incrementals (delete those first)")
	// ErrParentVolumeMismatch means the parent backup belongs to a different volume.
	ErrParentVolumeMismatch = errors.New("volume: parent backup is of a different volume")
)

// blockManifest is the ordered per-block hash list of an image, plus the block
// size and total image size it was computed at.
type blockManifest struct {
	BlockSize int
	ImageSize int64
	Hashes    [][32]byte
}

// blockLen is the byte length of block index within an image of imageSize at
// blockSize — full blocks except a possibly-short final block.
func blockLen(index, imageSize, blockSize int64) int64 {
	off := index * blockSize
	if off+blockSize > imageSize {
		return imageSize - off
	}
	return blockSize
}

// backupManifestPath is the sidecar manifest path for a backup's data file
// (<id>.img or <id>.delta → <id>.manifest).
func backupManifestPath(dataPath string) string {
	return strings.TrimSuffix(dataPath, filepath.Ext(dataPath)) + ".manifest"
}

// computeManifest hashes every block of the file at path.
func computeManifest(path string, blockSize int) (*blockManifest, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return nil, err
	}
	imageSize := fi.Size()
	bs := int64(blockSize)
	nBlocks := (imageSize + bs - 1) / bs
	hashes := make([][32]byte, nBlocks)
	buf := make([]byte, bs)
	for i := int64(0); i < nBlocks; i++ {
		blen := blockLen(i, imageSize, bs)
		if _, err := io.ReadFull(in, buf[:blen]); err != nil {
			return nil, fmt.Errorf("volume: read block %d of %s: %w", i, path, err)
		}
		hashes[i] = sha256.Sum256(buf[:blen])
	}
	return &blockManifest{BlockSize: blockSize, ImageSize: imageSize, Hashes: hashes}, nil
}

// writeManifest serializes a manifest to path atomically (temp + rename).
func writeManifest(path string, m *blockManifest) error {
	hdr := make([]byte, 8+4+8+8)
	copy(hdr, manifestMagic)
	binary.BigEndian.PutUint32(hdr[8:], uint32(m.BlockSize))
	binary.BigEndian.PutUint64(hdr[12:], uint64(m.ImageSize))
	binary.BigEndian.PutUint64(hdr[20:], uint64(len(m.Hashes)))
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("volume: create manifest: %w", err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	for i := range m.Hashes {
		if _, err := f.Write(m.Hashes[i][:]); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("volume: finalize manifest: %w", err)
	}
	ok = true
	return nil
}

// readManifest deserializes a manifest sidecar. os.ErrNotExist when absent.
func readManifest(path string) (*blockManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, 8+4+8+8)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("volume: read manifest header: %w", err)
	}
	if string(hdr[:8]) != manifestMagic {
		return nil, fmt.Errorf("volume: bad manifest magic in %s", path)
	}
	m := &blockManifest{
		BlockSize: int(binary.BigEndian.Uint32(hdr[8:])),
		ImageSize: int64(binary.BigEndian.Uint64(hdr[12:])),
	}
	if m.BlockSize <= 0 || m.BlockSize > maxBackupBlockSize {
		return nil, fmt.Errorf("volume: manifest %s has an implausible block size %d", path, m.BlockSize)
	}
	if m.ImageSize < 0 {
		return nil, fmt.Errorf("volume: manifest %s has a negative image size", path)
	}
	n := binary.BigEndian.Uint64(hdr[20:])
	// A corrupt count must not drive a huge allocation: cap it, then require the
	// file to actually hold that many hashes before allocating.
	if n > maxManifestBlocks {
		return nil, fmt.Errorf("volume: manifest %s hash count %d exceeds the %d ceiling", path, n, maxManifestBlocks)
	}
	if fi, ferr := f.Stat(); ferr == nil && fi.Size() < int64(len(hdr))+int64(n)*32 {
		return nil, fmt.Errorf("volume: manifest %s too small for %d hashes", path, n)
	}
	m.Hashes = make([][32]byte, n)
	for i := uint64(0); i < n; i++ {
		if _, err := io.ReadFull(f, m.Hashes[i][:]); err != nil {
			return nil, fmt.Errorf("volume: read manifest hash %d: %w", i, err)
		}
	}
	return m, nil
}

// writeDelta reads curPath, writes to deltaPath only the blocks whose hash
// differs from parent (or lie beyond the parent's range, i.e. the image grew),
// and returns the current image's full manifest.
func writeDelta(curPath string, parent *blockManifest, deltaPath string) (*blockManifest, error) {
	in, err := os.Open(curPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return nil, err
	}
	imageSize := fi.Size()
	bs := int64(parent.BlockSize)
	nBlocks := (imageSize + bs - 1) / bs

	out, err := os.OpenFile(deltaPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("volume: create delta: %w", err)
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(deltaPath)
		}
	}()

	hdr := make([]byte, deltaHeader)
	copy(hdr, deltaMagic)
	binary.BigEndian.PutUint32(hdr[8:], uint32(parent.BlockSize))
	binary.BigEndian.PutUint64(hdr[12:], uint64(imageSize))
	// nChanged (hdr[20:28]) is backfilled after the pass.
	if _, err := out.Write(hdr); err != nil {
		return nil, err
	}

	hashes := make([][32]byte, nBlocks)
	buf := make([]byte, bs)
	idx := make([]byte, 8)
	var nChanged uint64
	for i := int64(0); i < nBlocks; i++ {
		blen := blockLen(i, imageSize, bs)
		if _, err := io.ReadFull(in, buf[:blen]); err != nil {
			return nil, fmt.Errorf("volume: read block %d: %w", i, err)
		}
		h := sha256.Sum256(buf[:blen])
		hashes[i] = h
		if i >= int64(len(parent.Hashes)) || h != parent.Hashes[i] {
			binary.BigEndian.PutUint64(idx, uint64(i))
			if _, err := out.Write(idx); err != nil {
				return nil, err
			}
			if _, err := out.Write(buf[:blen]); err != nil {
				return nil, err
			}
			nChanged++
		}
	}
	binary.BigEndian.PutUint64(idx, nChanged)
	if _, err := out.WriteAt(idx, 20); err != nil {
		return nil, err
	}
	if err := out.Sync(); err != nil {
		return nil, err
	}
	ok = true
	return &blockManifest{BlockSize: parent.BlockSize, ImageSize: imageSize, Hashes: hashes}, nil
}

// applyDelta applies deltaPath's changed blocks onto targetPath, then truncates
// it to the delta's recorded image size (so growth/shrink across the chain is
// reproduced exactly).
func applyDelta(targetPath, deltaPath string) error {
	in, err := os.Open(deltaPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	hdr := make([]byte, deltaHeader)
	if _, err := io.ReadFull(in, hdr); err != nil {
		return fmt.Errorf("%w: read delta header: %v", ErrBackupChainBroken, err)
	}
	if string(hdr[:8]) != deltaMagic {
		return fmt.Errorf("%w: bad delta magic in %s", ErrBackupChainBroken, deltaPath)
	}
	bs := int64(binary.BigEndian.Uint32(hdr[8:]))
	imageSize := int64(binary.BigEndian.Uint64(hdr[12:]))
	nChanged := binary.BigEndian.Uint64(hdr[20:])
	// The delta may be an untrusted imported stream: bound the block size (it sizes
	// a buffer) and reject a negative image size before allocating or truncating.
	if bs <= 0 || bs > maxBackupBlockSize {
		return fmt.Errorf("%w: implausible delta block size %d", ErrBackupChainBroken, bs)
	}
	if imageSize < 0 {
		return fmt.Errorf("%w: negative delta image size", ErrBackupChainBroken)
	}

	out, err := os.OpenFile(targetPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	nBlocks := (imageSize + bs - 1) / bs
	idx := make([]byte, 8)
	buf := make([]byte, bs)
	for j := uint64(0); j < nChanged; j++ {
		if _, err := io.ReadFull(in, idx); err != nil {
			return fmt.Errorf("%w: read delta index: %v", ErrBackupChainBroken, err)
		}
		i := int64(binary.BigEndian.Uint64(idx))
		// A hostile/corrupt index must not drive a negative or out-of-range slice.
		if i < 0 || i >= nBlocks {
			return fmt.Errorf("%w: delta block index %d out of range [0,%d)", ErrBackupChainBroken, i, nBlocks)
		}
		blen := blockLen(i, imageSize, bs)
		if _, err := io.ReadFull(in, buf[:blen]); err != nil {
			return fmt.Errorf("%w: read delta block %d: %v", ErrBackupChainBroken, i, err)
		}
		if _, err := out.WriteAt(buf[:blen], i*bs); err != nil {
			return err
		}
	}
	if err := out.Truncate(imageSize); err != nil {
		return err
	}
	return out.Sync()
}

// verifyImage checks the file at path hashes block-for-block to man.
func verifyImage(path string, man *blockManifest) error {
	got, err := computeManifest(path, man.BlockSize)
	if err != nil {
		return err
	}
	if got.ImageSize != man.ImageSize || len(got.Hashes) != len(man.Hashes) {
		return fmt.Errorf("%w: reconstructed image size/blocks differ", ErrBackupChainBroken)
	}
	for i := range man.Hashes {
		if got.Hashes[i] != man.Hashes[i] {
			return fmt.Errorf("%w: block %d mismatch after reconstruction", ErrBackupChainBroken, i)
		}
	}
	return nil
}

// ensureManifest returns rec's block manifest, computing + caching it from the
// backup's image the first time (a full backup's manifest is lazy — created only
// when its first child incremental needs it). An incremental always has a
// manifest sidecar from creation.
func (m *Manager) ensureManifest(rec BackupRecord) (*blockManifest, error) {
	manPath := backupManifestPath(rec.Path)
	man, err := readManifest(manPath)
	if err == nil {
		return man, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if rec.Kind == backupKindIncremental {
		// An incremental's manifest can't be recomputed from its .delta alone.
		return nil, fmt.Errorf("%w: manifest missing for incremental %s", ErrBackupChainBroken, rec.ID)
	}
	bs := rec.BlockSize
	if bs == 0 {
		bs = defaultBackupBlockSize
	}
	man, err = computeManifest(rec.Path, bs)
	if err != nil {
		return nil, err
	}
	if err := writeManifest(manPath, man); err != nil {
		return nil, err
	}
	return man, nil
}

// BackupIncremental records only the blocks of volume name that changed since
// parentID (a prior backup of the same volume), plus a manifest — a chain the
// tip of which RestoreTo can reassemble. Like Backup, it does NOT verify the
// volume is quiescent (the daemon handler classifies that). ErrBackupNotFound if
// the parent is gone, ErrParentVolumeMismatch if it belongs to another volume.
func (m *Manager) BackupIncremental(name, parentID string) (BackupRecord, error) {
	if !nameRe.MatchString(name) {
		return BackupRecord{}, ErrInvalidName
	}
	rec, ok, err := m.st.get(name)
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok {
		return BackupRecord{}, ErrNotFound
	}
	parent, ok, err := m.st.getBackup(parentID)
	if err != nil {
		return BackupRecord{}, err
	}
	if !ok {
		return BackupRecord{}, ErrBackupNotFound
	}
	if parent.SourceVolume != name {
		return BackupRecord{}, ErrParentVolumeMismatch
	}
	if err := m.Sync(name); err != nil {
		return BackupRecord{}, err
	}
	parentMan, err := m.ensureManifest(parent)
	if err != nil {
		return BackupRecord{}, err
	}

	id := name + "-" + time.Now().UTC().Format("20060102T150405.000Z")
	destDir := filepath.Join(m.backupDir, name)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return BackupRecord{}, fmt.Errorf("volume: create backup dir %s: %w", destDir, err)
	}
	deltaPath := filepath.Join(destDir, id+".delta")
	curMan, err := writeDelta(filepath.Join(m.dir, name+".img"), parentMan, deltaPath)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("volume: incremental backup %s: %w", name, err)
	}
	if err := writeManifest(backupManifestPath(deltaPath), curMan); err != nil {
		_ = os.Remove(deltaPath)
		return BackupRecord{}, err
	}
	brec := BackupRecord{
		ID: id, SourceVolume: name, SizeBytes: rec.SizeBytes,
		CreatedAt: time.Now().UTC(), Consistency: "filesystem", HostID: m.hostID, Path: deltaPath,
		Encrypted: rec.Encrypted, WrappedKey: rec.WrappedKey, KeyID: rec.KeyID,
		Kind: backupKindIncremental, ParentID: parentID, BlockSize: parentMan.BlockSize,
	}
	if err := m.st.putBackup(brec); err != nil {
		_ = os.Remove(deltaPath)
		_ = os.Remove(backupManifestPath(deltaPath))
		return BackupRecord{}, err
	}
	return brec, nil
}

// resolveChain returns the backups from the base full (index 0) through the
// incremental id (last), following ParentID. ErrBackupChainBroken on a missing
// link, a cycle, or no base full.
func (m *Manager) resolveChain(id string) ([]BackupRecord, error) {
	var chain []BackupRecord
	seen := map[string]bool{}
	for cur := id; cur != ""; {
		if seen[cur] {
			return nil, fmt.Errorf("%w: cycle at %s", ErrBackupChainBroken, cur)
		}
		seen[cur] = true
		rec, ok, err := m.st.getBackup(cur)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: missing %s", ErrBackupChainBroken, cur)
		}
		chain = append(chain, rec)
		if rec.Kind != backupKindIncremental {
			break // reached the base full
		}
		cur = rec.ParentID
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if len(chain) == 0 || chain[0].Kind == backupKindIncremental {
		return nil, fmt.Errorf("%w: no base full backup for %s", ErrBackupChainBroken, id)
	}
	return chain, nil
}

// reconstructChain materializes the full image for the chain's tip into tmpPath:
// copy the base full, apply each delta in order, then verify against the tip's
// manifest when its sidecar is present (a same-host chain; an imported chain has
// no sidecar, so verification is skipped).
func (m *Manager) reconstructChain(chain []BackupRecord, tmpPath string) error {
	if err := fsutil.Clone(chain[0].Path, tmpPath); err != nil {
		return fmt.Errorf("volume: reconstruct base: %w", err)
	}
	for _, inc := range chain[1:] {
		if err := applyDelta(tmpPath, inc.Path); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}
	tip := chain[len(chain)-1]
	man, err := readManifest(backupManifestPath(tip.Path))
	if err == nil {
		if verr := verifyImage(tmpPath, man); verr != nil {
			_ = os.Remove(tmpPath)
			return verr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// backupChildren returns the ids of incrementals whose parent is rec.
func (m *Manager) backupChildren(rec BackupRecord) ([]string, error) {
	all, err := m.st.listBackups()
	if err != nil {
		return nil, err
	}
	var kids []string
	for _, b := range all {
		if b.ParentID == rec.ID {
			kids = append(kids, b.ID)
		}
	}
	return kids, nil
}
