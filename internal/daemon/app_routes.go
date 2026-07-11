package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/sdk/api"
)

// App routes expose durable apps (v0.4): a named workload the daemon
// reconciles into a running instance and re-creates after a restart. Apps
// are addressed by name (the stable DNS-label handle); the app id is
// internal. All routes answer 501 when no AppManager is configured.

func (s *Server) appsEnabled(w http.ResponseWriter) bool {
	if s.cfg.AppManager == nil {
		writeError(w, http.StatusNotImplemented, errors.New("apps are not enabled on this daemon"))
		return false
	}
	return true
}

// handleCreateApp — POST /apps. Body is a CreateAppRequest. The reconciler
// boots the instance asynchronously; the response is the app's initial
// state (its instance may still be pending).
func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.CreateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	desiredRunning := req.DesiredState != "stopped"
	rec, err := s.cfg.AppManager.Create(req.AppSpec, desiredRunning)
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	resp, err := s.cfg.AppManager.Get(rec.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleListApps — GET /apps.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	apps, err := s.cfg.AppManager.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AppListResponse{Apps: apps})
}

// handleGetApp — GET /apps/{name}.
func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	resp, err := s.cfg.AppManager.GetByName(r.PathValue("name"))
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteApp — DELETE /apps/{name}. Removes the app and tears down its
// instance on the next reconcile.
func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	if err := s.cfg.AppManager.DeleteByName(r.PathValue("name")); err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appErrStatus maps app-manager errors to HTTP statuses.
func appErrStatus(err error) int {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, app.ErrNameTaken):
		return http.StatusConflict
	default:
		// validation (bad name/image/policy) is a 400; everything else 500.
		if isValidationErr(err) {
			return http.StatusBadRequest
		}
		return http.StatusInternalServerError
	}
}

// isValidationErr reports the app-spec validation failures, which are
// plain errors.New from validateSpec (no sentinel). A 400 for those, 500
// otherwise. Kept narrow: only the known validation prefixes.
func isValidationErr(err error) bool {
	msg := err.Error()
	for _, p := range []string{"app: invalid name", "app: image is required", "app: unknown restart policy"} {
		if len(msg) >= len(p) && msg[:len(p)] == p {
			return true
		}
	}
	return false
}
