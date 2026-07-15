package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gnana997/crucible/internal/secretstore"
	"github.com/gnana997/crucible/sdk/api"
)

// Secret routes (v0.7.4) manage encrypted secret bundles: a named set of
// key→value pairs, sealed at rest, injected into an app's env with envFrom
// (AppSpec.SecretEnvFrom). The API is WRITE-ONLY — no route ever returns a
// value; reads yield only names. Gated by the default-deny `secret` scoped op.

func (s *Server) secretsEnabled(w http.ResponseWriter) bool {
	if s.cfg.SecretStore == nil {
		writeError(w, http.StatusNotImplemented,
			errors.New("secrets are not enabled on this daemon (set --secrets-key-file or CRUCIBLE_SECRETS_KEY)"))
		return false
	}
	return true
}

// handlePutSecret — PUT /secrets/{name}. Body is a SecretRequest. Replaces the
// bundle with Data, or (Merge) sets/updates the given keys.
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	if !secretstore.ValidName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid secret name %q (want a DNS label)", name))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.SecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("secret bundle must have at least one key"))
		return
	}
	var err error
	if req.Merge {
		err = s.cfg.SecretStore.SetKeys(name, req.Data)
	} else {
		err = s.cfg.SecretStore.Set(name, req.Data)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSecrets — GET /secrets. Bundle names only.
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	names, err := s.cfg.SecretStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SecretListResponse{Secrets: names})
}

// handleGetSecretKeys — GET /secrets/{name}. The bundle's key NAMES only, never
// values.
func (s *Server) handleGetSecretKeys(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	name := r.PathValue("name")
	keys, found, err := s.cfg.SecretStore.Keys(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no secret bundle %q", name))
		return
	}
	writeJSON(w, http.StatusOK, api.SecretKeysResponse{Name: name, Keys: keys})
}

// handleDeleteSecret — DELETE /secrets/{name}.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	if err := s.cfg.SecretStore.Delete(r.PathValue("name")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
