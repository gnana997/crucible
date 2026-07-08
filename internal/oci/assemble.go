package oci

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/gnana997/crucible/internal/version"
)

// Assembly caps — flag-tunable at the daemon layer; these are the
// defaults. They bound what a hostile or degenerate image can make the
// converter chew through.
const (
	DefaultMaxContentBytes = 8 << 30 // 8 GiB of uncompressed content
	DefaultMaxEntries      = 2_000_000
	DefaultMaxLayers       = 500
)

// Injected paths. "crucible/" is a reserved namespace: image-provided
// entries under it are dropped (and counted) so the injected files win
// deterministically, regardless of how a downstream tar consumer
// handles duplicate names.
const (
	reservedPrefix  = "crucible"
	injectedAgent   = "crucible/crucible-agent"
	injectedRunJSON = "crucible/run.json"
	// InjectedAgentPath is the in-image path of the injected agent
	// (no leading slash), for callers building an init= boot arg.
	InjectedAgentPath = injectedAgent
	injectedDirMode   = 0o755
	injectedExecMode  = 0o755
	injectedFileMode  = 0o644
)

// AssembleOptions configures Assemble. Agent is required; zero values
// elsewhere take the defaults above.
type AssembleOptions struct {
	// Agent is the static crucible-agent binary injected at
	// /crucible/crucible-agent (the daemon passes its embedded copy).
	Agent []byte

	MaxContentBytes int64
	MaxEntries      int
	MaxLayers       int

	// ConverterVersion stamps run.json; empty means the build version.
	ConverterVersion string

	// Now stamps run.json's conversion time; nil means time.Now. Fixing
	// it (tests) makes assembly output byte-for-byte deterministic.
	Now func() time.Time
}

// AssembleStats reports what the assembler wrote and what it dropped.
// Anything skipped is counted — coverage gaps must be visible, never
// silent.
type AssembleStats struct {
	// Entries and ContentBytes cover everything written, injected
	// files included. ContentBytes counts regular-file payloads — the
	// input to ext4 sizing in materialize.
	Entries      int
	ContentBytes int64

	// SkippedDevices counts block/char device nodes dropped from the
	// image (devtmpfs provides /dev at boot; nothing hostile enters
	// the artifact). SkippedReserved counts image entries under the
	// reserved crucible/ namespace.
	SkippedDevices  int
	SkippedReserved int
}

// Assemble flattens the acquired image per the OCI changeset rules,
// hardens every entry, appends the injected agent + stamped run.json,
// and writes one well-formed tar stream to w. It touches no
// filesystem: input is the image, output is the stream — materialize
// decides what to do with it.
//
// Flattening is deliberately hand-rolled rather than delegated to
// ggcr's mutate.Extract, because empirically (v0.21.7, pinned by the
// tests) Extract does not apply opaque-dir whiteouts (.wh..wh..opq) —
// lower-layer files resurrect — and it emits hardlinks without
// validating their targets, so a hardlink to a whiteouted file
// (go-containerregistry#977) lands dangling in the output. ggcr still
// does what it is best at: registry protocol, media types, and layer
// decompression.
func Assemble(acq *Acquired, w io.Writer, opts AssembleOptions) (*AssembleStats, error) {
	if len(opts.Agent) == 0 {
		return nil, errors.New("oci: assemble: agent binary is required")
	}
	if opts.MaxContentBytes <= 0 {
		opts.MaxContentBytes = DefaultMaxContentBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxLayers <= 0 {
		opts.MaxLayers = DefaultMaxLayers
	}
	if opts.ConverterVersion == "" {
		opts.ConverterVersion = version.String()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	layers, err := acq.Image.Layers()
	if err != nil {
		return nil, fmt.Errorf("oci: read image layers: %w", err)
	}
	if len(layers) > opts.MaxLayers {
		return nil, fmt.Errorf("oci: image has %d layers, exceeding the %d cap", len(layers), opts.MaxLayers)
	}

	stats := &AssembleStats{}
	tw := tar.NewWriter(w)
	written := make(map[string]bool) // names written to the artifact
	links := make(map[string]string) // hardlink name -> target

	err = flattenLayers(layers, func(hdr *tar.Header, body io.Reader) error {
		name := hdr.Name // already normalized by the walker
		if name == reservedPrefix || strings.HasPrefix(name, reservedPrefix+"/") {
			stats.SkippedReserved++
			return nil
		}
		switch hdr.Typeflag {
		case tar.TypeChar, tar.TypeBlock:
			// Device nodes never enter the artifact; the guest gets
			// /dev from devtmpfs at boot.
			stats.SkippedDevices++
			return nil
		case tar.TypeLink:
			// A hardlink target is an in-archive path and must obey
			// the same rules as entry names. Existence is verified
			// after the walk, when the final name set is known.
			target, tskip, terr := normalizeEntryName(hdr.Linkname)
			if terr != nil || tskip {
				return fmt.Errorf("oci: hardlink %q has invalid target %q", name, hdr.Linkname)
			}
			hdr.Linkname = target
			links[name] = target
		}
		// Symlink targets pass through verbatim: they resolve inside
		// the guest at runtime and never touch the host in pipe mode;
		// the fallback extractor (materialize) is responsible for not
		// following them during staging.

		// Numeric uid/gid are authoritative (Docker semantics); names
		// would tempt a tar consumer into remapping via the *host's*
		// passwd.
		hdr.Uname, hdr.Gname = "", ""

		if err := writeEntry(tw, body, hdr, stats, &opts); err != nil {
			return err
		}
		written[name] = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A dangling hardlink (target whiteouted, dropped, or absent —
	// go-containerregistry#977's failure class) must fail the
	// conversion rather than ship a broken filesystem.
	for name, target := range links {
		if !written[target] {
			return nil, fmt.Errorf("oci: hardlink %q targets %q, which is not in the flattened image", name, target)
		}
	}

	if err := injectArtifacts(tw, acq, stats, &opts); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("oci: finalize tar: %w", err)
	}
	return stats, nil
}

// whiteoutPrefix and opaqueMarker are the OCI layer changeset markers.
const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// flattenLayers walks the image's layers top-down, first-name-wins,
// applying whiteouts and opaque directories per the OCI image spec:
// a ".wh.<name>" entry deletes <name> (and its subtree) from all lower
// layers; a ".wh..wh..opq" entry masks the *lower layers'* content of
// its directory. Marker files themselves are never emitted. Entry
// names are normalized (and hostile ones rejected) before any rule is
// evaluated. emit receives each surviving entry exactly once.
func flattenLayers(layers []v1.Layer, emit func(hdr *tar.Header, body io.Reader) error) error {
	seen := make(map[string]bool) // names emitted or shadowed by upper layers
	var tombstones []string       // whiteouted subtree roots from upper layers
	var opaqueDirs []string       // opaque dirs from upper layers ("" = root)

	for i := len(layers) - 1; i >= 0; i-- {
		rc, err := layers[i].Uncompressed()
		if err != nil {
			return fmt.Errorf("oci: open layer %d: %w", i, err)
		}
		// Markers found in THIS layer apply only to layers below it;
		// collect them and merge after the layer is done.
		var layerTombs, layerOpaque []string

		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = rc.Close()
				return fmt.Errorf("oci: read layer %d: %w", i, err)
			}
			name, skip, err := normalizeEntryName(hdr.Name)
			if err != nil {
				_ = rc.Close()
				return fmt.Errorf("oci: entry %q: %w", hdr.Name, err)
			}
			if skip {
				continue
			}

			base := path.Base(name)
			if base == opaqueMarker {
				layerOpaque = append(layerOpaque, path.Dir(name))
				continue
			}
			if strings.HasPrefix(base, whiteoutPrefix) {
				layerTombs = append(layerTombs, path.Join(path.Dir(name), strings.TrimPrefix(base, whiteoutPrefix)))
				continue
			}

			if seen[name] || masked(name, tombstones, opaqueDirs) {
				continue
			}
			out := *hdr
			out.Name = name
			if err := emit(&out, tr); err != nil {
				_ = rc.Close()
				return err
			}
			seen[name] = true
		}
		if err := rc.Close(); err != nil {
			return fmt.Errorf("oci: close layer %d: %w", i, err)
		}
		tombstones = append(tombstones, layerTombs...)
		opaqueDirs = append(opaqueDirs, layerOpaque...)
	}
	return nil
}

// masked reports whether a lower-layer entry is hidden by an upper
// layer's whiteout tombstone or opaque directory.
func masked(name string, tombstones, opaqueDirs []string) bool {
	for _, t := range tombstones {
		if name == t || strings.HasPrefix(name, t+"/") {
			return true
		}
	}
	for _, o := range opaqueDirs {
		if o == "." || strings.HasPrefix(name, o+"/") {
			return true
		}
	}
	return false
}

// writeEntry writes one header (+payload for regular files) with cap
// accounting.
func writeEntry(tw *tar.Writer, body io.Reader, hdr *tar.Header, stats *AssembleStats, opts *AssembleOptions) error {
	stats.Entries++
	if stats.Entries > opts.MaxEntries {
		return fmt.Errorf("oci: image exceeds the %d-entry cap", opts.MaxEntries)
	}
	if hdr.Typeflag == tar.TypeReg {
		stats.ContentBytes += hdr.Size
		if stats.ContentBytes > opts.MaxContentBytes {
			return fmt.Errorf("oci: image content exceeds the %d-byte cap", opts.MaxContentBytes)
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("oci: write entry %q: %w", hdr.Name, err)
	}
	// body is nil for injected entries — their payloads are written by
	// the caller after the header.
	if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && body != nil {
		if _, err := io.CopyN(tw, body, hdr.Size); err != nil {
			return fmt.Errorf("oci: copy entry %q: %w", hdr.Name, err)
		}
	}
	return nil
}

// injectArtifacts appends the /crucible overlay: the directory, the
// agent, and run.json stamped with the conversion time and converter
// version. Injection comes last so it deterministically wins over any
// (already-dropped) image entries in the reserved namespace.
func injectArtifacts(tw *tar.Writer, acq *Acquired, stats *AssembleStats, opts *AssembleOptions) error {
	now := opts.Now().UTC()

	rc := *acq.RunConfig
	rc.ConvertedAtUnixMs = now.UnixMilli()
	rc.ConverterVersion = opts.ConverterVersion
	runJSON, err := json.MarshalIndent(&rc, "", "  ")
	if err != nil {
		return fmt.Errorf("oci: marshal run.json: %w", err)
	}
	runJSON = append(runJSON, '\n')

	entries := []struct {
		hdr  tar.Header
		body []byte
	}{
		{hdr: tar.Header{Typeflag: tar.TypeDir, Name: reservedPrefix, Mode: injectedDirMode, ModTime: now}},
		{hdr: tar.Header{Typeflag: tar.TypeReg, Name: injectedAgent, Mode: injectedExecMode, Size: int64(len(opts.Agent)), ModTime: now}, body: opts.Agent},
		{hdr: tar.Header{Typeflag: tar.TypeReg, Name: injectedRunJSON, Mode: injectedFileMode, Size: int64(len(runJSON)), ModTime: now}, body: runJSON},
	}
	for i := range entries {
		e := &entries[i]
		if err := writeEntry(tw, nil, &e.hdr, stats, opts); err != nil {
			return err
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				return fmt.Errorf("oci: write injected %q: %w", e.hdr.Name, err)
			}
		}
	}
	return nil
}

// normalizeEntryName validates and canonicalizes a tar entry name.
// Absolute paths and anything escaping the root are hostile input and
// fail the conversion; the root entry itself ("./", ".") is skipped.
func normalizeEntryName(name string) (clean string, skip bool, err error) {
	if name == "" {
		return "", true, nil
	}
	if strings.HasPrefix(name, "/") {
		return "", false, errors.New("absolute path in layer")
	}
	clean = path.Clean(name)
	if clean == "." {
		return "", true, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, errors.New("path escapes the image root")
	}
	return clean, false, nil
}
