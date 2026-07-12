package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gnana997/crucible/sdk/api"
)

// Registry routes manage private-registry pull credentials (v0.4.4). The store
// holds usable secrets, so: the secret is accepted only on POST and is never
// returned by any endpoint, and the routes answer 501 when no store is
// configured (pulls stay anonymous). Managing credentials is gated by the
// `registry` scoped-token operation.

func (s *Server) registryEnabled(w http.ResponseWriter) bool {
	if s.cfg.RegistryStore == nil {
		writeError(w, http.StatusNotImplemented, errors.New("registry credentials are not enabled on this daemon (set --registry-store)"))
		return false
	}
	return true
}

// handleRegistryLogin — POST /registry/credentials. Adds or replaces the
// credential for a registry host.
func (s *Server) handleRegistryLogin(w http.ResponseWriter, r *http.Request) {
	if !s.registryEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.RegistryCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.cfg.RegistryStore.Upsert(req.Host, req.Username, req.Secret); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleRegistryLogout — DELETE /registry/credentials/{host}.
func (s *Server) handleRegistryLogout(w http.ResponseWriter, r *http.Request) {
	if !s.registryEnabled(w) {
		return
	}
	host := r.PathValue("host")
	found, err := s.cfg.RegistryStore.Delete(host)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no credential for registry %q", host))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRegistryList — GET /registry/credentials. Host + username only; the
// store already zeroes the secret.
func (s *Server) handleRegistryList(w http.ResponseWriter, r *http.Request) {
	if !s.registryEnabled(w) {
		return
	}
	out := api.RegistryCredentialListResponse{}
	for _, c := range s.cfg.RegistryStore.List() {
		out.Registries = append(out.Registries, api.RegistryCredential{
			Host: c.Host, Username: c.Username, CreatedAt: c.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
