package daemon

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// Volume routes manage persistent block-device volumes (v0.6.0). All answer
// 501 when no volume storage is configured (--volume-dir).

func (s *Server) volumesEnabled(w http.ResponseWriter) bool {
	if s.cfg.Volumes == nil {
		writeError(w, http.StatusNotImplemented, errors.New("volumes are not enabled on this daemon (set --volume-dir)"))
		return false
	}
	return true
}

func toAPIVolume(i volume.Info) api.Volume {
	return api.Volume{
		Name:       i.Name,
		SizeBytes:  i.SizeBytes,
		CreatedAt:  i.CreatedAt,
		HostID:     i.HostID,
		AttachedTo: i.AttachedTo,
		Encrypted:  i.Encrypted,
	}
}

// handleCreateVolume — POST /volumes. Body is a CreateVolumeRequest.
func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.CreateVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := s.cfg.Volumes.Create(req.Name, req.SizeBytes, volume.CreateOpts{Encrypt: req.Encrypt})
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(volume.Info{Record: rec}))
}

// handleListVolumes — GET /volumes.
func (s *Server) handleListVolumes(w http.ResponseWriter, _ *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	infos, err := s.cfg.Volumes.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.Volume, 0, len(infos))
	for _, i := range infos {
		out = append(out, toAPIVolume(i))
	}
	writeJSON(w, http.StatusOK, api.VolumeListResponse{Volumes: out})
}

// handleGetVolume — GET /volumes/{name}.
func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	info, err := s.cfg.Volumes.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIVolume(info))
}

// handleDeleteVolume — DELETE /volumes/{name}. 409 if attached to a live sandbox.
func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	if err := s.cfg.Volumes.Remove(r.PathValue("name")); err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toAPIBackup(b volume.BackupRecord) api.Backup {
	return api.Backup{
		ID:           b.ID,
		SourceVolume: b.SourceVolume,
		SizeBytes:    b.SizeBytes,
		CreatedAt:    b.CreatedAt,
		Consistency:  b.Consistency,
		HostID:       b.HostID,
		Encrypted:    b.Encrypted,
	}
}

// handleShredVolume — POST /volumes/{name}/shred. Crypto-shreds an encrypted
// volume: destroys its keyslots and deletes the wrapped key, making the data
// permanently unrecoverable. 409 if attached to a live sandbox; 400 for a
// plaintext volume (use DELETE). Irreversible — gated by the delete op.
func (s *Server) handleShredVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	if err := s.cfg.Volumes.Shred(r.PathValue("name")); err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// volumeQuiescent reports whether a volume is safe to raw-copy: detached (no
// writer) or attached to a slept sandbox (VMM stopped, backing file
// host-fsync'd). A volume attached to a *running* sandbox is live — it must be
// frozen (fsfreeze) before copying. The second return is the holder sandbox id
// ("" when detached). Returns the Get error (ErrNotFound etc.) on lookup failure.
func (s *Server) volumeQuiescent(name string) (bool, string, error) {
	info, err := s.cfg.Volumes.Get(name)
	if err != nil {
		return false, "", err
	}
	if info.AttachedTo == "" {
		return true, "", nil
	}
	if s.cfg.Manager == nil {
		return false, info.AttachedTo, nil
	}
	asleep, known := s.cfg.Manager.Asleep(info.AttachedTo)
	return known && asleep, info.AttachedTo, nil
}

var errBackupNeedsReflink = errors.New(
	"volume is attached to a running sandbox and the backup filesystem is not reflink-capable; " +
		"sleep the app first, or put the backup dir on a btrfs/XFS filesystem")

// withFrozen freezes the live holder's volName filesystem (so a copy is
// filesystem-consistent), runs fn, then ALWAYS thaws. Refused when the backup
// filesystem is not reflink-capable — freezing the guest for a full byte copy is
// too disruptive, so only the O(1) reflink case is allowed live.
func (s *Server) withFrozen(ctx context.Context, volName, holder string, fn func() error) error {
	if !s.cfg.Volumes.BackupReflinks() {
		return errBackupNeedsReflink
	}
	if s.cfg.Manager == nil {
		return errBackupNeedsReflink
	}
	if err := s.cfg.Manager.Freeze(ctx, holder, volName); err != nil {
		return err
	}
	// Thaw on a fresh context so a cancelled request still unfreezes the guest
	// (the agent's watchdog is the last-resort backstop).
	defer func() { _ = s.cfg.Manager.Thaw(context.Background(), holder, volName) }()
	return fn()
}

// backupErrStatus maps live-copy errors to statuses, falling back to the volume
// error map.
func backupErrStatus(err error) int {
	switch {
	case errors.Is(err, errBackupNeedsReflink), errors.Is(err, sandbox.ErrFreezeUnsupported):
		return http.StatusConflict
	default:
		return volumeErrStatus(err)
	}
}

// handleBackupVolume — POST /volumes/{name}/backups. Takes a consistent
// point-in-time copy: a detached/slept volume is copied directly; a live one is
// FIFREEZEd for the O(1) reflink copy (409 if the backup FS can't reflink).
func (s *Server) handleBackupVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	name := r.PathValue("name")
	q, holder, err := s.volumeQuiescent(name)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	var rec volume.BackupRecord
	if q {
		rec, err = s.cfg.Volumes.Backup(name)
	} else {
		err = s.withFrozen(r.Context(), name, holder, func() error {
			var e error
			rec, e = s.cfg.Volumes.Backup(name)
			return e
		})
	}
	if err != nil {
		writeError(w, backupErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIBackup(rec))
}

// handleRestoreVolume — POST /volumes/{name}/restore. Materialises backup From
// into the new volume {name}. 409 if {name} already exists (restore never
// overwrites).
func (s *Server) handleRestoreVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.RestoreVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := s.cfg.Volumes.RestoreTo(req.From, r.PathValue("name"))
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(volume.Info{Record: rec}))
}

// handleCloneVolume — POST /volumes/{name}/clone. Copies the (quiescent) volume
// {name} into the new volume To. 409 if the source is live or To exists.
func (s *Server) handleCloneVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.CloneVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src := r.PathValue("name")
	q, holder, err := s.volumeQuiescent(src)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	var rec volume.Record
	if q {
		rec, err = s.cfg.Volumes.Clone(src, req.To)
	} else {
		err = s.withFrozen(r.Context(), src, holder, func() error {
			var e error
			rec, e = s.cfg.Volumes.Clone(src, req.To)
			return e
		})
	}
	if err != nil {
		writeError(w, backupErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(volume.Info{Record: rec}))
}

// handleListBackups — GET /volumes/{name}/backups (one volume) and GET /backups
// (all). name is "" for the latter.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	recs, err := s.cfg.Volumes.ListBackups(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.Backup, 0, len(recs))
	for _, b := range recs {
		out = append(out, toAPIBackup(b))
	}
	writeJSON(w, http.StatusOK, api.BackupListResponse{Backups: out})
}

// handleDeleteBackup — DELETE /backups/{id}.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	if err := s.cfg.Volumes.DeleteBackup(r.PathValue("id")); err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleExportBackup — GET /backups/{id}/export[?compress=gzip|none]. Streams a
// backup's backing file off the host so the control plane can ship it to an
// object store; the daemon stays provider-agnostic. Default gzip (the .img is a
// sparse ext4 image, so gzip collapses the holes over the wire); `none` streams
// raw for a caller that dedups its own way. X-Crucible-Backup-Size carries the
// decompressed byte size (what import reconstructs); Content-Length is set only
// for the raw stream. Gated by the default-deny volume_backup op.
func (s *Server) handleExportBackup(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	id := r.PathValue("id")
	f, rec, size, err := s.cfg.Volumes.OpenBackup(id)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	defer func() { _ = f.Close() }()

	gzipped := r.URL.Query().Get("compress") != "none"
	w.Header().Set("X-Crucible-Backup-Size", strconv.FormatInt(size, 10))
	w.Header().Set("X-Crucible-Backup-Consistency", rec.Consistency)
	if gzipped {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.img.gz"`)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.img"`)
	}
	// Headers (and 200) are committed on the first write; a mid-stream failure
	// can only be signalled by truncating the body (the CP verifies the size),
	// so log it, mirroring handleAdminBackup.
	if !gzipped {
		if _, err := io.Copy(w, f); err != nil {
			s.cfg.Logger.Error("volume backup export failed mid-stream", "backup", id, "err", err)
		}
		return
	}
	gz := gzip.NewWriter(w)
	if _, err := io.Copy(gz, f); err != nil {
		s.cfg.Logger.Error("volume backup export failed mid-stream", "backup", id, "err", err)
		return
	}
	if err := gz.Close(); err != nil {
		s.cfg.Logger.Error("volume backup export gzip close failed", "backup", id, "err", err)
	}
}

// handleImportBackup — POST /backups/import?source=<vol>&consistency=<c>&compress=gzip|none.
// Streams a backup's bytes onto the host (the CP pushes them from an object
// store during restore) and registers a record; RestoreTo then materialises a
// volume. The body is an unbounded stream (a backup is large), so it is NOT
// size-capped. Default gzip, symmetric with export. Gated by volume_backup.
func (s *Server) handleImportBackup(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	source := r.URL.Query().Get("source")
	if source == "" {
		writeError(w, http.StatusBadRequest, errors.New("import: ?source=<volume> is required"))
		return
	}
	meta := volume.ImportMeta{
		SourceVolume: source,
		Consistency:  r.URL.Query().Get("consistency"),
		Compressed:   r.URL.Query().Get("compress") != "none",
	}
	rec, err := s.cfg.Volumes.ImportBackup(meta, r.Body)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIBackup(rec))
}

func volumeErrStatus(err error) int {
	switch {
	case errors.Is(err, volume.ErrNotFound), errors.Is(err, volume.ErrBackupNotFound):
		return http.StatusNotFound
	case errors.Is(err, volume.ErrExists), errors.Is(err, volume.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, volume.ErrInvalidName):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
