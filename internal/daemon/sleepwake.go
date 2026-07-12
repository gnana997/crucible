package daemon

// Sandbox-level in-place sleep/wake routes (v0.5.0 B3/B4). These are the
// low-level primitive, parallel to snapshot/fork; the product-facing surface is
// app-level sleep/wake (drives these through the app control plane). Not yet in
// the public API docs while scale-to-zero is under construction.

import (
	"errors"
	"net/http"
	"time"

	"github.com/gnana997/crucible/internal/sandbox"
)

func (s *Server) handleSleepSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	// A large-guest snapshot can outrun the write deadline; clear it like the
	// snapshot handler does. r.Context() still bounds the request.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	if _, err := s.cfg.Manager.SleepInPlace(r.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": "asleep"})
}

func (s *Server) handleWakeSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	if err := s.cfg.Manager.WakeInPlace(r.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": "running"})
}
