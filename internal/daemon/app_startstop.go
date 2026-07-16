package daemon

import (
	"net/http"
	"time"
)

// stopWaitTimeout bounds how long POST /apps/{name}/stop waits for the instance
// to actually tear down before returning. The desired-state flip has already
// happened; this just lets the caller (and the volume-grow recipe) observe the
// instance gone — and its volume detached — before proceeding.
const stopWaitTimeout = 60 * time.Second

// handleStopApp — POST /apps/{name}/stop. Flips the app to desired-stopped: the
// reconciler destroys the running instance (a COLD stop that detaches any
// volume, unlike sleep which keeps the snapshot + the single-writer guard). The
// spec is retained, so `start` boots it again. Waits (bounded) for the instance
// to actually go away so the volume is detached on return — the grow/backup
// recipe depends on that. Returns the updated app status.
func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	if err := s.cfg.AppManager.SetDesiredByName(name, false); err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	// The reconcile is asynchronous; poll until the instance is torn down (its
	// volume released) or the wait budget/request context expires.
	deadline := time.Now().Add(stopWaitTimeout)
	for {
		resp, err := s.cfg.AppManager.GetByName(name)
		if err != nil {
			writeError(w, appErrStatus(err), err)
			return
		}
		if resp.Status == nil || resp.Status.InstanceID == "" {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			// Best-effort: the stop is in flight; return the current state
			// rather than fail. The caller can poll `app get`.
			writeJSON(w, http.StatusOK, resp)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// handleStartApp — POST /apps/{name}/start. Flips the app to desired-running;
// the reconciler boots a fresh instance (re-attaching any volume at its current
// size). Booting is asynchronous — the response reflects the transition, and the
// app comes up via reconcile. Returns the updated app status.
func (s *Server) handleStartApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	if err := s.cfg.AppManager.SetDesiredByName(name, true); err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	resp, err := s.cfg.AppManager.GetByName(name)
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
