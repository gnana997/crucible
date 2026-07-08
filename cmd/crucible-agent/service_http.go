//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxServiceRequestBody bounds service request bodies. Specs are JSON —
// a cmd, env, and a handful of knobs — nothing large.
const maxServiceRequestBody = 1 << 20 // 1 MiB

// serviceAPI exposes the supervisor over the agent's vsock HTTP server.
type serviceAPI struct {
	sup *supervisor
}

func (a *serviceAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("PUT /service", a.handleConfigure)
	mux.HandleFunc("POST /service/start", a.handleStart)
	mux.HandleFunc("POST /service/stop", a.handleStop)
	mux.HandleFunc("POST /service/restart", a.handleRestart)
	mux.HandleFunc("GET /service/status", a.handleStatus)
	mux.HandleFunc("GET /service/logs", a.handleLogs)
}

// serviceLogsMaxBytes caps one logs read; serviceLogsDefaultBytes is
// used when the request doesn't say. JSON base64 inflates payloads by
// 4/3, so the cap keeps responses comfortably under a few MiB.
const (
	serviceLogsDefaultBytes = 256 << 10
	serviceLogsMaxBytes     = 1 << 20
)

func (a *serviceAPI) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fromSeq uint64
	if v := q.Get("from_seq"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			http.Error(w, "invalid from_seq", http.StatusBadRequest)
			return
		}
		fromSeq = n
	}
	maxBytes := serviceLogsDefaultBytes
	if v := q.Get("max_bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid max_bytes", http.StatusBadRequest)
			return
		}
		maxBytes = min(n, serviceLogsMaxBytes)
	}

	resp, err := a.sup.Logs(fromSeq, maxBytes)
	if err != nil {
		if errors.Is(err, errNoServiceSpec) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *serviceAPI) handleConfigure(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxServiceRequestBody)
	var spec agentwire.ServiceSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	status, err := a.sup.Configure(&spec)
	a.respond(w, status, err)
}

func (a *serviceAPI) handleStart(w http.ResponseWriter, _ *http.Request) {
	status, err := a.sup.Start()
	a.respond(w, status, err)
}

func (a *serviceAPI) handleStop(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxServiceRequestBody)
	var req agentwire.ServiceStopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	if req.GraceSec < 0 {
		http.Error(w, "grace_s must be >= 0", http.StatusBadRequest)
		return
	}
	status, err := a.sup.Stop(time.Duration(req.GraceSec) * time.Second)
	a.respond(w, status, err)
}

func (a *serviceAPI) handleRestart(w http.ResponseWriter, _ *http.Request) {
	status, err := a.sup.Restart()
	a.respond(w, status, err)
}

func (a *serviceAPI) handleStatus(w http.ResponseWriter, _ *http.Request) {
	status, err := a.sup.Status()
	a.respond(w, status, err)
}

// respond maps supervisor errors onto HTTP statuses: state conflicts
// (nothing configured) are 409, validation problems 400, and a
// shut-down supervisor 503. Success returns the ServiceStatus JSON so
// every mutation shows the caller the state it produced.
func (a *serviceAPI) respond(w http.ResponseWriter, status agentwire.ServiceStatus, err error) {
	switch {
	case err == nil:
	case errors.Is(err, errNoServiceSpec):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, errSupervisorDown):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Best-effort: headers are gone, an encode failure has no recovery.
	_ = json.NewEncoder(w).Encode(status)
}
