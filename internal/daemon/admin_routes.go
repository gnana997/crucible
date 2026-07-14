package daemon

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gnana997/crucible/internal/version"
)

// backupManifest is the last entry in a daemon backup tarball: what was
// captured, by which daemon, when — so a restore can sanity-check versions
// before laying files down.
type backupManifest struct {
	CrucibleVersion string    `json:"crucible_version"`
	CreatedAt       time.Time `json:"created_at"`
	Hostname        string    `json:"hostname"`
	Entries         []string  `json:"entries"`
}

// handleAdminBackup streams a tar.gz of the daemon's persistent state:
// the app store (bbolt), the token file, the volume-record store (bbolt),
// and the registry-credential file — each present only when its component is
// configured — plus a trailing manifest.json. The bolt stores are copied via
// a read transaction (consistent, hot — the daemon keeps serving); the two
// JSON files are read under their own locks. Cross-store skew is accepted and
// documented: the stores are independent.
//
// Volume DATA is deliberately absent — that's `volume backup`'s job; this is
// the metadata half that makes a restored host know its apps, tokens,
// volumes, and pull credentials.
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="crucible-backup.tar.gz"`)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	var entries []string

	// After the first byte is written the status line is gone; a mid-stream
	// failure can only be reported by truncating the stream (tar/gzip fail
	// loudly on extraction). Log it and stop.
	abort := func(what string, err error) {
		s.cfg.Logger.Error("admin backup failed mid-stream", "entry", what, "err", err)
	}

	writeBytes := func(name string, b []byte) bool {
		if b == nil {
			return true // component has no state yet — omit the entry
		}
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: now}
		if err := tw.WriteHeader(hdr); err != nil {
			abort(name, err)
			return false
		}
		if _, err := tw.Write(b); err != nil {
			abort(name, err)
			return false
		}
		entries = append(entries, name)
		return true
	}
	// frame adapts the tar writer to the bolt stores' BackupTo contract: the
	// exact size arrives from the snapshot-pinning read transaction, so the
	// header is written just-in-time.
	frame := func(name string) func(size int64) (io.Writer, error) {
		return func(size int64) (io.Writer, error) {
			hdr := &tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: now}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			entries = append(entries, name)
			return tw, nil
		}
	}

	if s.cfg.AppManager != nil {
		if err := s.cfg.AppManager.BackupStoreTo(frame("app.db")); err != nil {
			abort("app.db", err)
			return
		}
	}
	if s.cfg.TokenStore != nil {
		b, err := s.cfg.TokenStore.Dump()
		if err != nil {
			abort("tokens.json", err)
			return
		}
		if !writeBytes("tokens.json", b) {
			return
		}
	}
	if s.cfg.Volumes != nil {
		if err := s.cfg.Volumes.BackupStoreTo(frame("volume-index.db")); err != nil {
			abort("volume-index.db", err)
			return
		}
	}
	if s.cfg.RegistryStore != nil {
		b, err := s.cfg.RegistryStore.Dump()
		if err != nil {
			abort("registry-credentials.json", err)
			return
		}
		if !writeBytes("registry-credentials.json", b) {
			return
		}
	}

	host, _ := os.Hostname()
	mb, err := json.MarshalIndent(backupManifest{
		CrucibleVersion: version.String(),
		CreatedAt:       now,
		Hostname:        host,
		Entries:         entries,
	}, "", "  ")
	if err != nil {
		abort("manifest.json", err)
		return
	}
	if !writeBytes("manifest.json", mb) {
		return
	}

	if err := tw.Close(); err != nil {
		abort("tar close", err)
		return
	}
	if err := gz.Close(); err != nil {
		abort("gzip close", err)
	}
}
