package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
	rec, err := s.cfg.Volumes.Create(req.Name, req.SizeBytes)
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
	}
}

// handleBackupVolume — POST /volumes/{name}/backups. Takes a consistent
// point-in-time copy. Refuses (409) a volume attached to a *running* sandbox: a
// detached or slept (VMM stopped) volume is quiescent and safe to copy, but a
// live one is still writing — a no-downtime live backup needs the fsfreeze agent
// op, which lands in a later milestone.
func (s *Server) handleBackupVolume(w http.ResponseWriter, r *http.Request) {
	if !s.volumesEnabled(w) {
		return
	}
	name := r.PathValue("name")
	info, err := s.cfg.Volumes.Get(name)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	if info.AttachedTo != "" {
		asleep, known := false, false
		if s.cfg.Manager != nil {
			asleep, known = s.cfg.Manager.Asleep(info.AttachedTo)
		}
		if !known || !asleep {
			writeError(w, http.StatusConflict, errors.New("volume is attached to a running sandbox; sleep the app first (no-downtime live backup lands in a later release)"))
			return
		}
	}
	rec, err := s.cfg.Volumes.Backup(name)
	if err != nil {
		writeError(w, volumeErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIBackup(rec))
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
