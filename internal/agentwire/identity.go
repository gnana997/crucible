package agentwire

// IdentityRefreshRequest is the JSON body of POST /identity/refresh.
// Sent by the host to a freshly-forked VM so it wakes with unique
// state instead of a byte-for-byte copy of the snapshot's.
type IdentityRefreshRequest struct {
	// Seed is 32 bytes of host-CSPRNG entropy, unique per fork
	// (encoding/json carries []byte as base64 on the wire). The agent
	// credits it to the guest kernel's entropy pool and forces a CRNG
	// reseed, so per-fork RNG divergence holds on any guest kernel,
	// with or without VMGenID support.
	Seed []byte `json:"seed"`

	// SandboxID is the fork's sandbox ID. Becomes the guest's hostname
	// and the content of the /run/crucible/fork-id marker.
	SandboxID string `json:"sandbox_id"`
}
