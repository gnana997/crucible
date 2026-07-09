package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
)

// Store is the content-addressed cache of converted images. Each image
// lives at <dir>/sha256_<hex>/{rootfs.ext4,image.json}, keyed by the
// source image's manifest digest, so pulling the same image twice
// dedupes to one artifact.
//
// The store directory MUST live outside the daemon's --work-base: the
// sandbox reconcile sweep reaps everything under work-base it doesn't
// recognize, and must never touch the image cache.
type Store struct {
	dir      string
	agent    []byte
	mode     MaterializeMode
	pullOpts []PullOption
	log      *slog.Logger

	sf singleflight.Group
	mu sync.RWMutex
	// byDigest is the in-memory index, rebuilt from disk on New.
	byDigest map[string]*ImageRecord
}

// ImageRecord describes one converted image. It is both the in-memory
// index entry and the on-disk image.json (RootfsPath is recomputed on
// load, not trusted from disk).
type ImageRecord struct {
	// Digest is the source image's manifest digest ("sha256:…") — the
	// store's identity for the image.
	Digest string `json:"digest"`

	// SourceRef is where it came from (a pull reference or the
	// docker-archive marker).
	SourceRef string `json:"source_ref"`

	// RootfsPath is the absolute path to the bootable ext4. Not
	// serialized — set on load/create from the store layout.
	RootfsPath string `json:"-"`

	// SizeBytes is the ext4 image's apparent size; ContentBytes and
	// Entries describe what went into it.
	SizeBytes    int64 `json:"size_bytes"`
	ContentBytes int64 `json:"content_bytes"`
	Entries      int   `json:"entries"`

	// ConvertMode is "pipe" or "staging"; ConvertedAtUnixMs stamps the
	// conversion.
	ConvertMode       string `json:"convert_mode"`
	ConvertedAtUnixMs int64  `json:"converted_at_unix_ms"`

	// RunConfig is the image's runtime contract — the source the boot
	// path computes the effective service spec from.
	RunConfig *RunConfig `json:"run_config"`
}

// StoreConfig configures New.
type StoreConfig struct {
	// Dir is the image cache root. Created if absent. Must be outside
	// the daemon work-base.
	Dir string

	// Agent is the static crucible-agent injected into every image.
	// Required.
	Agent []byte

	// Mode is the materialize path. Callers probe once
	// (ProbeTarballSupport) and pass the result.
	Mode MaterializeMode

	// PullOptions are applied to every registry pull (e.g.
	// WithInsecureRegistry for a LAN or test registry).
	PullOptions []PullOption

	// Logger; nil uses slog.Default.
	Logger *slog.Logger
}

const (
	rootfsName   = "rootfs.ext4"
	recordName   = "image.json"
	digestDirPfx = "sha256_"
)

// New opens (creating if needed) an image store and rebuilds its index
// by scanning the directory. Incomplete image dirs (missing rootfs or
// record) are left in place but not indexed — Delete/manual cleanup
// handles them; a future write to the same digest overwrites cleanly.
func New(cfg StoreConfig) (*Store, error) {
	if len(cfg.Agent) == 0 {
		return nil, errors.New("oci: store: agent binary is required")
	}
	if cfg.Dir == "" {
		return nil, errors.New("oci: store: dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("oci: create image dir: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dir:      cfg.Dir,
		agent:    cfg.Agent,
		mode:     cfg.Mode,
		pullOpts: cfg.PullOptions,
		log:      log.With("component", "images"),
		byDigest: make(map[string]*ImageRecord),
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.log.Info("image store ready", "dir", cfg.Dir, "images", len(s.byDigest), "mode", cfg.Mode.String())
	return s, nil
}

// scan rebuilds byDigest from the store directory.
func (s *Store) scan() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("oci: scan image dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), digestDirPfx) {
			continue
		}
		dir := filepath.Join(s.dir, e.Name())
		rec, err := loadRecord(dir)
		if err != nil {
			s.log.Warn("skipping unreadable image dir", "dir", e.Name(), "err", err)
			continue
		}
		s.byDigest[rec.Digest] = rec
	}
	return nil
}

func loadRecord(dir string) (*ImageRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, recordName))
	if err != nil {
		return nil, err
	}
	var rec ImageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	rootfs := filepath.Join(dir, rootfsName)
	if _, err := os.Stat(rootfs); err != nil {
		return nil, fmt.Errorf("rootfs missing: %w", err)
	}
	if rec.Digest == "" {
		return nil, errors.New("record has no digest")
	}
	rec.RootfsPath = rootfs
	return &rec, nil
}

// Pull fetches and converts a registry image, deduping on digest.
func (s *Store) Pull(ctx context.Context, ref string) (*ImageRecord, error) {
	acq, err := Pull(ctx, ref, s.pullOpts...)
	if err != nil {
		return nil, err
	}
	return s.convert(ctx, acq)
}

// Import spools a docker-save stream to a temp file and converts it.
// tag disambiguates a multi-image archive; empty requires a single
// image.
func (s *Store) Import(ctx context.Context, r io.Reader, tag string) (*ImageRecord, error) {
	tmp, err := os.CreateTemp(s.dir, ".import-*.tar")
	if err != nil {
		return nil, fmt.Errorf("oci: temp for import: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("oci: spool import archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("oci: close import archive: %w", err)
	}
	acq, err := ImportDockerArchive(tmpPath, tag)
	if err != nil {
		return nil, err
	}
	return s.convert(ctx, acq)
}

// convert materializes an acquired image into the store under
// singleflight so concurrent identical requests build it once. An
// already-present digest returns immediately without re-downloading
// layers.
func (s *Store) convert(ctx context.Context, acq *Acquired) (*ImageRecord, error) {
	if rec := s.lookupDigest(acq.Digest); rec != nil {
		return rec, nil
	}
	v, err, _ := s.sf.Do(acq.Digest, func() (any, error) {
		if rec := s.lookupDigest(acq.Digest); rec != nil {
			return rec, nil
		}
		return s.materializeAndRecord(ctx, acq)
	})
	if err != nil {
		return nil, err
	}
	return v.(*ImageRecord), nil
}

func (s *Store) materializeAndRecord(ctx context.Context, acq *Acquired) (*ImageRecord, error) {
	dir := filepath.Join(s.dir, digestDir(acq.Digest))
	// Clean any partial remains from a prior failed conversion.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("oci: clear image dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("oci: create image dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()

	rootfs := filepath.Join(dir, rootfsName)
	res, err := Materialize(ctx, acq, rootfs, MaterializeOptions{
		AssembleOptions: AssembleOptions{Agent: s.agent},
		Mode:            s.mode,
		ScratchDir:      dir,
	})
	if err != nil {
		return nil, err
	}

	rec := &ImageRecord{
		Digest:            acq.Digest,
		SourceRef:         acq.SourceRef,
		RootfsPath:        rootfs,
		SizeBytes:         res.SizeBytes,
		ContentBytes:      res.Stats.ContentBytes,
		Entries:           res.Stats.Entries,
		ConvertMode:       res.Mode,
		ConvertedAtUnixMs: acq.RunConfig.ConvertedAtUnixMs,
		RunConfig:         acq.RunConfig,
	}
	// Materialize stamps the injected run.json but not our copy; mirror
	// the conversion time onto the record for a consistent view.
	if rec.ConvertedAtUnixMs == 0 && rec.RunConfig != nil {
		rec.ConvertedAtUnixMs = rec.RunConfig.ConvertedAtUnixMs
	}
	if err := writeRecord(dir, rec); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.byDigest[rec.Digest] = rec
	s.mu.Unlock()
	success = true
	s.log.Info("image converted", "digest", rec.Digest, "ref", rec.SourceRef,
		"size_bytes", rec.SizeBytes, "mode", rec.ConvertMode)
	return rec, nil
}

func writeRecord(dir string, rec *ImageRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("oci: marshal image record: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, recordName), data, 0o640); err != nil {
		return fmt.Errorf("oci: write image record: %w", err)
	}
	return nil
}

// List returns all images, newest conversion first.
func (s *Store) List() []*ImageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ImageRecord, 0, len(s.byDigest))
	for _, rec := range s.byDigest {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConvertedAtUnixMs != out[j].ConvertedAtUnixMs {
			return out[i].ConvertedAtUnixMs > out[j].ConvertedAtUnixMs
		}
		return out[i].Digest < out[j].Digest
	})
	return out
}

// ErrImageNotFound is returned by Get/Delete for an unknown reference.
var ErrImageNotFound = errors.New("image not found")

// ErrAmbiguousImage is returned when a short reference matches more
// than one image.
var ErrAmbiguousImage = errors.New("ambiguous image reference")

// Get resolves an image by full digest, digest hex, a unique hex
// prefix, or an exact source ref.
func (s *Store) Get(ref string) (*ImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolveLocked(ref)
}

func (s *Store) resolveLocked(ref string) (*ImageRecord, error) {
	if ref == "" {
		return nil, ErrImageNotFound
	}
	if rec, ok := s.byDigest[ref]; ok {
		return rec, nil
	}
	hexRef := strings.TrimPrefix(ref, "sha256:")
	var matches []*ImageRecord
	for digest, rec := range s.byDigest {
		hex := strings.TrimPrefix(digest, "sha256:")
		if hex == hexRef || rec.SourceRef == ref {
			return rec, nil
		}
		if strings.HasPrefix(hex, hexRef) {
			matches = append(matches, rec)
		}
	}
	switch len(matches) {
	case 0:
		return nil, ErrImageNotFound
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: %q matches %d images", ErrAmbiguousImage, ref, len(matches))
	}
}

// Delete removes an image and its on-disk artifacts.
func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.resolveLocked(ref)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, digestDir(rec.Digest))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("oci: remove image dir: %w", err)
	}
	delete(s.byDigest, rec.Digest)
	s.log.Info("image deleted", "digest", rec.Digest, "ref", rec.SourceRef)
	return nil
}

func (s *Store) lookupDigest(digest string) *ImageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byDigest[digest]
}

// digestDir maps "sha256:<hex>" to the filesystem-safe "sha256_<hex>".
func digestDir(digest string) string {
	return strings.Replace(digest, ":", "_", 1)
}
