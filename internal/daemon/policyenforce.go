package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gnana997/crucible/internal/policy"
)

// policyCtxKey keys the presenting token's policy in the request context. A nil
// *policy.Policy (or an absent value) means unrestricted — an unscoped key, or
// an unauthenticated loopback request.
type policyCtxKey struct{}

func withPolicy(ctx context.Context, p *policy.Policy) context.Context {
	return context.WithValue(ctx, policyCtxKey{}, p)
}

// policyFor returns the enforced policy for r, or nil when the request is
// unrestricted (unscoped token, or auth disabled).
func policyFor(r *http.Request) *policy.Policy {
	p, _ := r.Context().Value(policyCtxKey{}).(*policy.Policy)
	return p
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
		return policy.OpRead, true
	case http.MethodDelete:
		return policy.OpDelete, true
	case http.MethodPost:
		switch {
		case path == "/sandboxes":
			return policy.OpCreate, true
		case strings.HasSuffix(path, "/exec"):
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
