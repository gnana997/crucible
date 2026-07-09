package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Materialize sizing knobs.
const (
	// sizePadFloorBytes is the minimum free space added on top of the
	// image content — headroom for ext4 metadata, the journal, and a
	// little runtime writable space (growable-disk sizing is future work).
	sizePadFloorBytes = 256 << 20 // 256 MiB
	// sizePadNumer/sizePadDenom express the proportional pad (20%).
	sizePadNumer = 1
	sizePadDenom = 5
	// sizeAlign rounds the image up to a whole number of MiB.
	sizeAlign = 1 << 20
	// bytesPerInodeDefault mirrors mke2fs's default ratio; we only pass
	// an explicit inode count when the image's entry count would
	// otherwise exhaust the default budget (many-tiny-files images).
	bytesPerInodeDefault = 16384
	// inodeSlack pads the computed inode count.
	inodeSlack = 4096
	// mkfsTimeout / validateTimeout bound the external tools so a wedged
	// mkfs/fsck can't hang a conversion forever.
	mkfsTimeout     = 10 * time.Minute
	validateTimeout = 5 * time.Minute
)

// MaterializeMode selects how the assembled tar becomes an ext4 image.
type MaterializeMode int

const (
	// ModePipe feeds the tarball straight to `mkfs.ext4 -d` (e2fsprogs
	// ≥ 1.47.1 with libarchive). Untrusted content never becomes host
	// files.
	ModePipe MaterializeMode = iota
	// ModeStaging extracts the (already-hardened) tar to a scratch dir
	// and runs `mkfs.ext4 -d <dir>`. For hosts whose mkfs lacks tarball
	// support. Faithful uid/gid preservation needs root (the daemon has
	// it); the extractor is defense-in-depth on pre-screened input.
	ModeStaging
)

func (m MaterializeMode) String() string {
	if m == ModeStaging {
		return "staging"
	}
	return "pipe"
}

// MaterializeOptions configures Materialize.
type MaterializeOptions struct {
	AssembleOptions

	// Mode picks the conversion path. Callers probe once at startup
	// (ProbeTarballSupport) and pass the result to every conversion.
	Mode MaterializeMode

	// ScratchDir is where the temp tar and (staging mode) the extract
	// dir live. Should be on the same filesystem as the destination so
	// the final rename is atomic. Empty uses the destination's dir.
	ScratchDir string
}

// MaterializeResult reports a finished conversion.
type MaterializeResult struct {
	Path      string // the ext4 image (== the requested destination)
	SizeBytes int64  // apparent (allocated) image size
	Mode      string
	Stats     *AssembleStats
}

// Materialize converts an acquired image into a bootable ext4 artifact
// at destPath. It assembles once into a temp tar (which measures exact
// content size on the same pass), sizes and builds the ext4 via the
// selected mode, validates it (fsck + injected-file checks), and
// atomically renames it into place. destPath is untouched on any error.
func Materialize(ctx context.Context, acq *Acquired, destPath string, opts MaterializeOptions) (*MaterializeResult, error) {
	scratch := opts.ScratchDir
	if scratch == "" {
		scratch = filepath.Dir(destPath)
	}
	work, err := os.MkdirTemp(scratch, ".crucible-convert-*")
	if err != nil {
		return nil, fmt.Errorf("oci: create scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	// 1. Assemble to a temp tar; AssembleStats gives exact sizing input.
	tarPath := filepath.Join(work, "rootfs.tar")
	stats, err := assembleToFile(acq, tarPath, opts.AssembleOptions)
	if err != nil {
		return nil, err
	}

	// 2. Size the image and create it sparse.
	size := computeImageSize(stats.ContentBytes)
	imgPath := filepath.Join(work, "rootfs.ext4")
	if err := truncateSparse(imgPath, size); err != nil {
		return nil, err
	}

	// 3. Build the filesystem.
	source := tarPath
	if opts.Mode == ModeStaging {
		stageDir := filepath.Join(work, "stage")
		if err := extractTar(tarPath, stageDir); err != nil {
			return nil, err
		}
		source = stageDir
	}
	if err := runMkfs(ctx, source, imgPath, stats.Entries, size); err != nil {
		return nil, err
	}

	// 4. Validate before publishing.
	if err := validateImage(ctx, imgPath); err != nil {
		return nil, err
	}

	// 5. Atomic publish.
	if err := os.Rename(imgPath, destPath); err != nil {
		return nil, fmt.Errorf("oci: publish image to %s: %w", destPath, err)
	}
	return &MaterializeResult{Path: destPath, SizeBytes: size, Mode: opts.Mode.String(), Stats: stats}, nil
}

// assembleToFile runs Assemble into a file, syncing before close so
// mkfs sees a complete tar.
func assembleToFile(acq *Acquired, path string, opts AssembleOptions) (*AssembleStats, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("oci: create temp tar: %w", err)
	}
	stats, aerr := Assemble(acq, f, opts)
	if aerr != nil {
		_ = f.Close()
		return nil, aerr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("oci: sync temp tar: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("oci: close temp tar: %w", err)
	}
	return stats, nil
}

// computeImageSize returns the ext4 image size: content plus the larger
// of a proportional pad and a floor, rounded up to a whole MiB.
func computeImageSize(contentBytes int64) int64 {
	pad := contentBytes * sizePadNumer / sizePadDenom
	if pad < sizePadFloorBytes {
		pad = sizePadFloorBytes
	}
	size := contentBytes + pad
	if rem := size % sizeAlign; rem != 0 {
		size += sizeAlign - rem
	}
	return size
}

// inodeCount returns the -N value for mkfs, or 0 to accept the default
// ratio. We override only when entries would exhaust the default
// budget, and cap so the inode table itself can't dominate the image.
func inodeCount(entries int, size int64) int64 {
	need := int64(entries) + int64(entries)/sizePadDenom + inodeSlack
	def := size / bytesPerInodeDefault
	if need <= def {
		return 0
	}
	// ~256 bytes/inode; keep the table under 1/8 of the image.
	if cap := size / 8 / 256; need > cap {
		need = cap
	}
	if need <= def {
		return 0
	}
	return need
}

func truncateSparse(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("oci: create image file: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("oci: size image file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("oci: close image file: %w", err)
	}
	return nil
}

// runMkfs builds an ext4 filesystem in imgPath from source (a tarball
// path in pipe mode, a directory in staging mode). -F forces operation
// on a regular file; -E root_owner=0:0 anchors the root inode; lazy
// init keeps the write sparse and fast.
func runMkfs(ctx context.Context, source, imgPath string, entries int, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, mkfsTimeout)
	defer cancel()

	args := []string{"-F", "-q", "-t", "ext4",
		"-E", "root_owner=0:0,lazy_itable_init=1,lazy_journal_init=1",
		"-d", source,
	}
	if n := inodeCount(entries, size); n > 0 {
		args = append(args, "-N", strconv.FormatInt(n, 10))
	}
	args = append(args, imgPath)

	out, err := exec.CommandContext(ctx, "mkfs.ext4", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("oci: mkfs.ext4 failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateImage fsck's the image read-only and confirms the injected
// artifacts are present and the agent is executable. Never mounts the
// (untrusted) filesystem — fsck and debugfs are userspace readers.
func validateImage(ctx context.Context, imgPath string) error {
	ctx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	// fsck.ext4 -f -n: full check, make no changes. Exit 0 == clean;
	// with -n, any detected problem yields a non-zero exit.
	out, err := exec.CommandContext(ctx, "fsck.ext4", "-f", "-n", imgPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("oci: fsck reported problems: %w: %s", err, strings.TrimSpace(string(out)))
	}

	agentStat, err := debugfsStat(ctx, imgPath, "/"+injectedAgent)
	if err != nil {
		return fmt.Errorf("oci: injected agent missing from image: %w", err)
	}
	if !strings.Contains(agentStat, "0755") && !strings.Contains(agentStat, "0100755") {
		return fmt.Errorf("oci: injected agent is not mode 0755: %s", firstLine(agentStat))
	}
	if _, err := debugfsStat(ctx, imgPath, "/"+injectedRunJSON); err != nil {
		return fmt.Errorf("oci: injected run.json missing from image: %w", err)
	}
	return nil
}

// debugfsStat runs `debugfs -R "stat <path>"` and returns its output,
// erroring if the file is absent.
func debugfsStat(ctx context.Context, imgPath, path string) (string, error) {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "stat "+path, imgPath).CombinedOutput()
	s := string(out)
	if err != nil {
		return "", fmt.Errorf("debugfs stat %s: %w: %s", path, err, strings.TrimSpace(s))
	}
	if strings.Contains(s, "File not found") || !strings.Contains(s, "Inode:") {
		return "", fmt.Errorf("debugfs: %s not present", path)
	}
	return s, nil
}

// ProbeTarballSupport reports whether the host's mkfs.ext4 accepts a
// tarball for -d (e2fsprogs ≥ 1.47.1 built with libarchive, libarchive
// present at runtime). It tests the actual operation — build a one-file
// tar, mkfs it into a tiny image, confirm the file landed — rather than
// parsing version strings. Callers probe once at startup and pass the
// resulting mode to Materialize.
func ProbeTarballSupport(ctx context.Context) bool {
	dir, err := os.MkdirTemp("", "crucible-probe-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tarPath := filepath.Join(dir, "probe.tar")
	if err := writeProbeTar(tarPath); err != nil {
		return false
	}
	imgPath := filepath.Join(dir, "probe.ext4")
	if err := truncateSparse(imgPath, 2<<20); err != nil {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(pctx, "mkfs.ext4", "-F", "-q", "-d", tarPath, imgPath).Run(); err != nil {
		return false
	}
	_, err = debugfsStat(pctx, imgPath, "/probe.txt")
	return err == nil
}

func writeProbeTar(path string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("crucible-probe")
	if err := tw.WriteHeader(&tar.Header{Name: "probe.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// extractTar unpacks a hardened tar into dir for staging-mode mkfs.
// Entry names were already validated by the assembler, but the walk
// re-checks defensively and never follows symlinks. Ownership is
// applied where permitted (root); non-root extraction still produces a
// functionally correct tree, just owned by the caller.
func extractTar(tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("oci: open temp tar: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("oci: create stage dir: %w", err)
	}

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("oci: read temp tar: %w", err)
		}
		clean, skip, nerr := normalizeEntryName(hdr.Name)
		if nerr != nil {
			return fmt.Errorf("oci: stage entry %q: %w", hdr.Name, nerr)
		}
		if skip {
			continue
		}
		if err := extractEntry(tr, hdr, dir, clean); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry writes one tar entry into the staging root. dir is the
// staging root; clean is the entry's normalized in-archive path. Mode
// and ownership are applied last (os.Chmod ignores umask, so setuid
// survives; os.Chown is best-effort — non-root can't set foreign uids,
// the documented staging limitation).
func extractEntry(tr *tar.Reader, hdr *tar.Header, dir, clean string) error {
	target := filepath.Join(dir, filepath.FromSlash(clean))
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
			_ = out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
		// A symlink's own mode is irrelevant; skip chmod, apply
		// ownership via Lchown so we don't follow the link.
		_ = os.Lchown(target, hdr.Uid, hdr.Gid)
		return nil
	case tar.TypeLink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Link(filepath.Join(dir, filepath.FromSlash(hdr.Linkname)), target); err != nil {
			return fmt.Errorf("oci: stage hardlink %s: %w", hdr.Name, err)
		}
		return nil // hardlink shares the target's inode/mode/owner
	case tar.TypeFifo:
		if err := mkfifo(target, uint32(os.FileMode(hdr.Mode).Perm())); err != nil {
			return fmt.Errorf("oci: stage fifo %s: %w", hdr.Name, err)
		}
	default:
		return nil
	}
	_ = os.Chown(target, hdr.Uid, hdr.Gid)
	if err := os.Chmod(target, os.FileMode(hdr.Mode)&os.ModePerm|setidBits(hdr.Mode)); err != nil {
		return fmt.Errorf("oci: chmod %s: %w", hdr.Name, err)
	}
	return nil
}

// setidBits extracts the setuid/setgid/sticky bits from a tar mode
// (raw octal, e.g. 0o4755) as Go FileMode bits, since FileMode.Perm
// masks them off.
func setidBits(tarMode int64) os.FileMode {
	var m os.FileMode
	if tarMode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if tarMode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if tarMode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
