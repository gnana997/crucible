package fcapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Error is returned by Client methods when the Firecracker API responds
// with a non-2xx status. It captures the HTTP status, the fault_message
// Firecracker typically includes in its error body, and the raw body
// (useful when Firecracker returns a shape we don't recognize).
type Error struct {
	// Status is the HTTP status code from the Firecracker API.
	Status int
	// FaultMessage is Firecracker's human-readable error string, parsed
	// from `{"fault_message": "..."}` in the response body. Empty if the
	// response body was not JSON or had no fault_message.
	FaultMessage string
	// RawBody is the original response body, retained for debugging.
	RawBody string
}

// Error implements the error interface. Format is stable enough to match
// on in tests: `firecracker API <status>: <message>`.
func (e *Error) Error() string {
	if e.FaultMessage != "" {
		return fmt.Sprintf("firecracker API %d: %s", e.Status, e.FaultMessage)
	}
	if e.RawBody != "" {
		return fmt.Sprintf("firecracker API %d: %s", e.Status, e.RawBody)
	}
	return fmt.Sprintf("firecracker API %d (no body)", e.Status)
}

// IsStatus reports whether err is a *Error with the given HTTP status.
// Useful in callers that want to react to e.g. 404 differently from 400.
func IsStatus(err error, status int) bool {
	var fcErr *Error
	if !errors.As(err, &fcErr) {
		return false
	}
	return fcErr.Status == status
}

// decodeError consumes the response body and builds a *Error from it.
// Safe to call on any non-2xx response; falls back to raw bytes if the
// body isn't the expected JSON envelope.
func decodeError(resp *http.Response) error {
	buf, _ := io.ReadAll(resp.Body)
	e := &Error{Status: resp.StatusCode, RawBody: string(buf)}

	var parsed struct {
		FaultMessage string `json:"fault_message"`
	}
	if json.Unmarshal(buf, &parsed) == nil && parsed.FaultMessage != "" {
		e.FaultMessage = parsed.FaultMessage
	}
	return e
}
