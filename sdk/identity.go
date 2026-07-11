package crucible

import "encoding/json"

// Identity is what the daemon reports about the caller's credential
// (GET /whoami). The type is SDK-owned rather than a re-export of the
// daemon's policy engine: today it describes a daemon API key's scope,
// and the shape can grow server-side (a future control-plane returns a
// user/tenant identity here) without this package importing daemon
// internals.
type Identity struct {
	// Scoped is false for an unscoped key or an unauthenticated
	// loopback daemon — i.e. the credential has full access.
	Scoped bool `json:"scoped"`

	// Policy is the effective policy document for a scoped token,
	// verbatim from the daemon (see docs/policy.md for the shape).
	// Nil when unscoped. Kept opaque here: callers that need the
	// structure decode it against the daemon version they target.
	Policy json.RawMessage `json:"policy,omitempty"`
}
