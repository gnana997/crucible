package crucible

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors for branching with errors.Is. Every daemon error is an
// *Error (use errors.As for the status and message); the common statuses
// additionally match a sentinel so callers don't hard-code HTTP codes.
var (
	// ErrNotFound matches a 404: the sandbox/snapshot/image doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized matches a 401: missing or invalid API key.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrPolicyDenied matches a 403: the token's policy forbids the
	// operation (or the request exceeds a policy cap).
	ErrPolicyDenied = errors.New("policy denied")
	// ErrConflict matches a 409: a name collision, an in-use resource, or a
	// lifecycle-state mismatch. Use Error.Code (see api.Code* constants) to
	// tell the specific conflict apart.
	ErrConflict = errors.New("conflict")
)

// Error is a structured error from the daemon. Status is the HTTP status
// code; Message is the daemon's {"error": ...} text. Code is reserved for
// machine-readable error codes (empty today) so a future control-plane
// can add quota/placement errors without a shape change.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("daemon returned %d: %s", e.Status, e.Message)
}

// Unwrap maps the status onto the package sentinels so
// errors.Is(err, ErrNotFound) works without unwrapping by hand.
func (e *Error) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrPolicyDenied
	case http.StatusConflict:
		return ErrConflict
	}
	return nil
}
