package daemon

import (
	"net/http"
	"time"
)

// handleSleepApp — POST /apps/{name}/sleep. Snapshots the app's current
// instance and stops its VMM to free RAM (scale-to-zero); the app stays
// addressable and wakes on demand. Returns the updated app status.
func (s *Server) handleSleepApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	// A large-guest snapshot can outrun the server write deadline; clear it as
	// the snapshot handler does. r.Context() still bounds the request.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	if err := s.cfg.AppManager.Sleep(r.Context(), name); err != nil {
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

// handleWakeApp — POST /apps/{name}/wake. Restores a slept app's instance in
// place (same id/netns/IP), reseeding its RNG and stepping its clock. Returns
// the updated app status (including last_wake_latency_ms).
func (s *Server) handleWakeApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	if err := s.cfg.AppManager.Wake(r.Context(), name); err != nil {
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
