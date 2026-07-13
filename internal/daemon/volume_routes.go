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

func volumeErrStatus(err error) int {
	switch {
	case errors.Is(err, volume.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, volume.ErrExists), errors.Is(err, volume.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, volume.ErrInvalidName):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
