//go:build linux

package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxFilesBody caps the PUT /files tar body. A push carries a user's own
// project into the guest; 1 GiB is generous and bounds a runaway upload. The
// daemon applies the same cap on its side.
const maxFilesBody = 1 << 30

// handleFilesPut extracts a tar streamed to PUT /files?path=<dest> into the
// guest filesystem beneath the (absolute) destination directory. It is a plain
// shared handler (no reaper): writing files is pure I/O and works identically
// in systemd-child and PID-1 boot positions.
//
// A push is the *user's own* files going into the box, so a malformed archive
// is contained inside the VM. We still reject entries that escape the
// destination (absolute paths, `..`, out-of-dest symlinks) as hygiene.
func handleFilesPut(w http.ResponseWriter, r *http.Request) {
	dest := r.URL.Query().Get("path")
	if dest == "" || !filepath.IsAbs(dest) {
		http.Error(w, "files: an absolute ?path= (destination dir in the guest) is required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFilesBody)

	res, err := extractTarInto(dest, r.Body)
	if err != nil {
		slog.Warn("files put failed", "dest", dest, "err", err)
		http.Error(w, fmt.Sprintf("files: %v", err), http.StatusBadRequest)
		return
	}
	slog.Info("files put", "dest", dest, "files", res.Files, "bytes", res.Bytes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// extractTarInto unpacks the tar in r beneath dest, creating dest first. Every
// entry is resolved against dest and refused if it escapes. Only regular
// files, directories, and in-dest symlinks are materialized; other entry types
// (devices, fifos, ...) are skipped.
func extractTarInto(dest string, r io.Reader) (agentwire.FilesPutResult, error) {
	var res agentwire.FilesPutResult
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return res, fmt.Errorf("create dest %q: %w", dest, err)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read archive: %w", err)
		}
		target, ok := safeJoin(dest, hdr.Name)
		if !ok {
			return res, fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return res, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return res, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return res, err
			}
			n, cErr := io.Copy(f, tr)
			if closeErr := f.Close(); cErr == nil {
				cErr = closeErr
			}
			if cErr != nil {
				return res, cErr
			}
			res.Files++
			res.Bytes += n
		case tar.TypeSymlink:
			if !symlinkStaysInDest(dest, target, hdr.Linkname) {
				return res, fmt.Errorf("archive symlink %q -> %q escapes the destination", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return res, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return res, err
			}
		default:
			// skip fifos / devices / char-special / hardlinks for push hygiene
		}
	}
	return res, nil
}

// safeJoin joins name onto dest and confirms the result stays within dest,
// rejecting absolute paths and `..` traversal. Returns the cleaned target and
// whether it is safe.
func safeJoin(dest, name string) (string, bool) {
	target := filepath.Join(dest, name)
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return target, true
}

// symlinkStaysInDest reports whether a symlink at linkPath pointing at linkname
// resolves to a location within dest. Absolute link targets are refused.
func symlinkStaysInDest(dest, linkPath, linkname string) bool {
	if filepath.IsAbs(linkname) {
		return false
	}
	resolved := filepath.Join(filepath.Dir(linkPath), linkname)
	rel, err := filepath.Rel(dest, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
