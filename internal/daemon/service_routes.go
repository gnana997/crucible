package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/wire"
)

// validateServiceSpec runs the shared structural validation plus the
// host-side signal-name check, so a bad spec fails the HTTP request
// instead of surfacing as an agent error mid-create.
func validateServiceSpec(spec *wire.ServiceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.StopSignal != "" && unix.SignalNum(spec.StopSignal) == 0 {
		return fmt.Errorf("service: unknown stop_signal %q", spec.StopSignal)
	}
	return nil
}

// serviceError maps Manager service-call failures onto HTTP statuses:
// unknown sandbox → 404, no spec configured yet → 409, an agent that
// predates the service API → 501, anything else → 500.
func (s *Server) serviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, agentapi.ErrNoServiceConfigured):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, agentapi.ErrServiceUnsupported):
		writeError(w, http.StatusNotImplemented, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) handleConfigureService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var spec wire.ServiceSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
		return
	}
	if err := validateServiceSpec(&spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := s.cfg.Manager.ConfigureService(r.Context(), id, &spec)
	if err != nil {
		s.serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, s.cfg.Manager.StartService)
}

func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, s.cfg.Manager.RestartService)
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request,
	action func(ctx context.Context, id string) (wire.ServiceStatus, error),
) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	status, err := action(r.Context(), id)
	if err != nil {
		s.serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req wire.ServiceStopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
		return
	}
	if req.GraceSec < 0 {
		writeError(w, http.StatusBadRequest, errors.New("grace_s must be >= 0"))
		return
	}
	status, err := s.cfg.Manager.StopService(r.Context(), id, req.GraceSec)
	if err != nil {
		s.serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	status, err := s.cfg.Manager.ServiceStatus(r.Context(), id)
	if err != nil {
		s.serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	q := r.URL.Query()
	var fromSeq uint64
	if v := q.Get("from_seq"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid from_seq"))
			return
		}
		fromSeq = n
	}
	var maxBytes int
	if v := q.Get("max_bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("invalid max_bytes"))
			return
		}
		maxBytes = n
	}
	logs, err := s.cfg.Manager.ServiceLogs(r.Context(), id, fromSeq, maxBytes)
	if err != nil {
		s.serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}
