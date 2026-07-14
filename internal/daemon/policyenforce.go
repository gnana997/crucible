package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/tokenstore"
)

// identityCtxKey keys the verified caller (token id + policy) in the request
// context. Absent means unrestricted — an unauthenticated loopback request.
type identityCtxKey struct{}

func withIdentity(ctx context.Context, id tokenstore.Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// policyFor returns the enforced policy for r, or nil when the request is
// unrestricted (unscoped token, or auth disabled).
func policyFor(r *http.Request) *policy.Policy {
	id, _ := r.Context().Value(identityCtxKey{}).(tokenstore.Identity)
	return id.Policy
}

// tokenIDFor returns the id of the API key behind r, or "" when unauthenticated.
// Sandboxes are stamped with it so per-token quotas can count them.
func tokenIDFor(r *http.Request) string {
	id, _ := r.Context().Value(identityCtxKey{}).(tokenstore.Identity)
	return id.TokenID
}

// enforcePolicy gates each request by the presenting token's allowed operations.
// It runs after auth (which attaches the policy). Resource ceilings — caps,
// network, profiles, timeout — are enforced in the handlers, which parse the
// body; this middleware handles only the coarse operation verb.
func (s *Server) enforcePolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pol := policyFor(r); pol != nil {
			if op, gated := operationFor(r.Method, r.URL.Path); gated && !pol.Allows(op) {
				writeError(w, http.StatusForbidden, fmt.Errorf("this token is not permitted to %s", op))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// operationFor classifies a request into a policy operation. The bool is false
// for endpoints that carry no operation gate (/healthz, /whoami) — reachable by
// any authenticated token regardless of its operation list.
func operationFor(method, path string) (policy.Operation, bool) {
	if path == "/whoami" || path == "/healthz" {
		return "", false
	}
	switch method {
	case http.MethodGet:
		// GET /sandboxes/{id}/exec is the WebSocket interactive exec —
		// running commands in the guest, not a read.
		if strings.HasSuffix(path, "/exec") {
			return policy.OpExec, true
		}
		// GET /sandboxes/{id}/capture streams the guest's packets — it exposes
		// traffic payloads, so it is its own default-deny op, not a read.
		if strings.HasSuffix(path, "/capture") {
			return policy.OpCapture, true
		}
		// GET /admin/backup streams the daemon's stores — token state
		// and usable registry secrets. Its own default-deny op, like capture.
		if path == "/admin/backup" {
			return policy.OpAdminBackup, true
		}
		// GET /backups/{id}/export streams a volume backup's DATA off the host.
		// Its own default-deny op, not a read.
		if strings.HasSuffix(path, "/export") {
			return policy.OpVolumeBackup, true
		}
		return policy.OpRead, true
	case http.MethodDelete:
		// Removing a registry credential is credential management, not a
		// resource delete — gate it like adding one.
		if strings.HasPrefix(path, "/registry/credentials") {
			return policy.OpRegistry, true
		}
		return policy.OpDelete, true
	case http.MethodPut:
		// PUT /sandboxes/{id}/service configures what runs in the guest —
		// exec-grade power.
		if strings.HasSuffix(path, "/service") {
			return policy.OpExec, true
		}
		// PUT /apps/{name} replaces the app's entrypoint spec — exec-grade,
		// same as creating one.
		if strings.HasPrefix(path, "/apps/") {
			return policy.OpExec, true
		}
	case http.MethodPost:
		switch {
		case path == "/sandboxes":
			return policy.OpCreate, true
		// Creating an app configures an entrypoint the daemon runs — exec-grade.
		case path == "/apps":
			return policy.OpExec, true
		// Storing a registry credential is credential management.
		case path == "/registry/credentials":
			return policy.OpRegistry, true
		// Image pull/import provision a bootable rootfs — create-grade.
		case path == "/images" || path == "/images/import":
			return policy.OpCreate, true
		// Creating a persistent volume — create-grade.
		case path == "/volumes":
			return policy.OpCreate, true
		// Backing up a volume copies its data out — snapshot-grade (a dedicated
		// default-deny volume_backup op lands in a later milestone).
		case strings.HasSuffix(path, "/backups"):
			return policy.OpSnapshot, true
		// Restore/clone provision a new volume — create-grade.
		case strings.HasSuffix(path, "/restore"), strings.HasSuffix(path, "/clone"):
			return policy.OpCreate, true
		case strings.HasSuffix(path, "/exec"):
			return policy.OpExec, true
		// Writing files into a sandbox is exec-grade power over the guest.
		case strings.HasSuffix(path, "/files"):
			return policy.OpExec, true
		// Service lifecycle mutations control the guest's entrypoint —
		// gated like exec.
		case strings.HasSuffix(path, "/service/start"),
			strings.HasSuffix(path, "/service/stop"),
			strings.HasSuffix(path, "/service/restart"):
			return policy.OpExec, true
		case strings.HasSuffix(path, "/snapshot"):
			return policy.OpSnapshot, true
		case strings.HasSuffix(path, "/fork"):
			return policy.OpFork, true
		}
	}
	return "", false
}

// handleWhoami lets a client (CLI `policy show`, the MCP server's tool mirror,
// a future UI) discover exactly what its token may do. It carries no operation
// gate, so even a token with an empty operation set can introspect itself.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	pol := policyFor(r)
	writeJSON(w, http.StatusOK, policy.Whoami{Scoped: pol != nil, Policy: pol})
}
