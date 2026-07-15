package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

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
	if err := s.checkAppEgressPolicy(r, req.Network); err != nil {
		writeError(w, http.StatusForbidden, err)
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

// checkAppEgressPolicy applies the scoped-token egress ceiling to an app's
// network request — an app's instance is created through the same path a
// sandbox is, so the network grants must apply here too (the sandbox-create
// handler is bypassed for apps). Shared by create and update. Nil policy (no
// token) permits everything, as elsewhere.
func (s *Server) checkAppEgressPolicy(r *http.Request, n *api.NetworkRequest) error {
	pol := policyFor(r)
	if pol == nil {
		return nil
	}
	var reqNet []string
	wantFull, wantCIDR := false, false
	if n != nil && n.Enabled {
		reqNet = n.Allowlist
		wantFull = n.FullEgress
		wantCIDR = len(n.AllowlistCIDR) > 0
	}
	return errors.Join(pol.CheckNetAllow(reqNet), pol.CheckFullEgress(wantFull, wantCIDR))
}

// handleUpdateApp — PUT /apps/{name}. Body is a full AppSpec (name immutable).
// Bumps the app's generation → the reconciler redeploys the instance from the
// new spec (destroy-then-boot). Desired running/stopped is retained (use the
// create/stopped path or a future desired-state route to change that).
func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var spec api.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if spec.Name == "" {
		spec.Name = name // the path is authoritative; a mismatch is rejected below
	}
	if err := s.checkAppEgressPolicy(r, spec.Network); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	rec, err := s.cfg.AppManager.Update(name, spec)
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	resp, err := s.cfg.AppManager.Get(rec.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
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

// handleListUsage — GET /usage. Every app's persistent usage metrics (including
// retained records for deleted apps) plus the reading's snapshot time.
func (s *Server) handleListUsage(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	writeJSON(w, http.StatusOK, api.UsageListResponse{
		Usage:            s.cfg.AppManager.AllUsage(),
		SnapshotUnixNano: time.Now().UnixNano(),
	})
}

// handleAppUsage — GET /apps/{name}/usage. One live app's usage, accrued to now.
func (s *Server) handleAppUsage(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	u, err := s.cfg.AppManager.AppUsage(r.PathValue("name"))
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleListAppDomains — GET /apps/{name}/domains.
func (s *Server) handleListAppDomains(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	domains, err := s.cfg.AppManager.ListDomains(r.PathValue("name"))
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, api.DomainListResponse{Domains: domains})
}

// handleAddAppDomain — POST /apps/{name}/domains, body {"domain": "..."}.
func (s *Server) handleAddAppDomain(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.AddDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := s.cfg.AppManager.AddDomain(r.PathValue("name"), req.Domain)
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	resp, err := s.cfg.AppManager.Get(rec.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRemoveAppDomain — DELETE /apps/{name}/domains/{domain}.
func (s *Server) handleRemoveAppDomain(w http.ResponseWriter, r *http.Request) {
	if !s.appsEnabled(w) {
		return
	}
	if _, err := s.cfg.AppManager.RemoveDomain(r.PathValue("name"), r.PathValue("domain")); err != nil {
		writeError(w, appErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// resolveAppInstance maps the {name} path value to the app's CURRENT instance
// (sandbox) id and stamps it as the request's {id} path value, so the delegated
// sandbox handler reads it transparently. Resolution is per-request, so an
// exec/logs call issued across a self-heal or rolling update targets whatever
// instance is current at call time — never a stale id captured by the client.
// Returns false (and writes the error) when apps are disabled (501), the app is
// unknown (404), or it has no running instance yet (409).
func (s *Server) resolveAppInstance(w http.ResponseWriter, r *http.Request) bool {
	if !s.appsEnabled(w) {
		return false
	}
	name := r.PathValue("name")
	resp, err := s.cfg.AppManager.GetByName(name)
	if err != nil {
		writeError(w, appErrStatus(err), err)
		return false
	}
	if resp.Status == nil || resp.Status.InstanceID == "" {
		writeError(w, http.StatusConflict, fmt.Errorf("app %s has no running instance", name))
		return false
	}
	r.SetPathValue("id", resp.Status.InstanceID)
	return true
}

// handleAppExec — POST /apps/{name}/exec (one-shot streaming, or ?stdin=1 for a
// hijacked interactive session). Resolves the app to its current instance, then
// delegates to the sandbox exec handler.
func (s *Server) handleAppExec(w http.ResponseWriter, r *http.Request) {
	if !s.resolveAppInstance(w, r) {
		return
	}
	s.handleExecSandbox(w, r)
}

// handleAppExecWS — GET /apps/{name}/exec: the WebSocket interactive exec
// (backing `crucible app shell`), resolved to the app's current instance.
func (s *Server) handleAppExecWS(w http.ResponseWriter, r *http.Request) {
	if !s.resolveAppInstance(w, r) {
		return
	}
	s.handleExecWS(w, r)
}

// handleAppLogs — GET /apps/{name}/logs: tail the current instance's durable
// logs (service + exec activity), resolved to the app's current instance.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	if !s.resolveAppInstance(w, r) {
		return
	}
	s.handleSandboxLogs(w, r)
}

// appErrStatus maps app-manager errors to HTTP statuses.
func appErrStatus(err error) int {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, app.ErrNameTaken):
		return http.StatusConflict
	case errors.Is(err, app.ErrNotRunning), errors.Is(err, app.ErrNotAsleep):
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
	for _, p := range []string{"app: invalid name", "app: image is required", "app: unknown restart policy", "app: name is immutable"} {
		if len(msg) >= len(p) && msg[:len(p)] == p {
			return true
		}
	}
	return false
}
