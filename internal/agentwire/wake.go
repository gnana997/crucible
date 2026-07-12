package agentwire

// WakeRequest is the JSON body of POST /wake. Sent by the host when restoring
// a slept VM in place (wake-in-place, v0.5.0). Unlike an identity refresh, wake
// deliberately PRESERVES the guest's identity — machine-id and hostname are
// left untouched, and the IP is unchanged. Only two things are corrected:
//
//   - the CRNG is reseeded (repeated wakes of one snapshot would otherwise
//     replay identical entropy state — a real cryptographic hazard), and
//   - the wall clock is stepped past the sleep gap.
//
// Both are applied before the guest is made reachable again, mirroring the
// fatal-before-reachable discipline of the fork identity refresh.
type WakeRequest struct {
	// Seed is 32 bytes of host-CSPRNG entropy, credited to the kernel entropy
	// pool with a forced CRNG reseed — the same mechanism as
	// IdentityRefreshRequest.Seed, without the identifier rotation.
	Seed []byte `json:"seed"`

	// WallTimeUnixNano is the host's current wall-clock time in Unix
	// nanoseconds. The guest steps CLOCK_REALTIME to it so TLS validation,
	// token/JWT expiry, and cache TTLs don't observe the (possibly
	// days-)stale time captured in the snapshot. Required (> 0).
	WallTimeUnixNano int64 `json:"wall_time_unix_nano"`
}
