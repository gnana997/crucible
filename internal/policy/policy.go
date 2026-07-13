// Package policy defines the per-token capability policy the daemon enforces on
// scoped API keys: what operations a token may perform, and the ceilings it may
// not exceed (network, profiles, resource caps). It is deliberately free of
// daemon/HTTP dependencies so the validation and the enforcement checks are pure
// and exhaustively unit-testable, and so both the daemon (authoritative) and the
// MCP server (mirror) can share one implementation.
//
// A policy attaches to one API key. All fields are optional; absent means "no
// restriction on that axis", so the zero Policy is fully permissive — an
// unscoped key. See docs/policy.md for the operator-facing schema.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gnana997/crucible/internal/netallow"
)

// Operation is a daemon-level verb a token may be allowed to perform. It maps to
// a set of REST endpoints (see docs/policy.md); the MCP server maps its tools
// back to these to decide which to advertise.
type Operation string

// The operation verbs. Each maps to a set of REST endpoints (comments below).
const (
	OpCreate   Operation = "create"   // POST /sandboxes
	OpExec     Operation = "exec"     // POST /sandboxes/{id}/exec
	OpSnapshot Operation = "snapshot" // POST /sandboxes/{id}/snapshot
	OpFork     Operation = "fork"     // POST /snapshots/{id}/fork
	OpDelete   Operation = "delete"   // DELETE /sandboxes|snapshots/{id}
	OpRead     Operation = "read"     // GET (list / inspect / profiles)
	OpRegistry Operation = "registry" // POST/DELETE /registry/credentials (manage private-registry creds)
	OpCapture  Operation = "capture"  // GET /sandboxes/{id}/capture (packet capture — exposes traffic payloads)
)

// KnownOperations returns the valid operation verbs, in a stable order (useful
// for error messages and docs).
func KnownOperations() []Operation {
	return []Operation{OpCreate, OpExec, OpSnapshot, OpFork, OpDelete, OpRead, OpRegistry, OpCapture}
}

func isKnownOp(op Operation) bool {
	for _, k := range KnownOperations() {
		if op == k {
			return true
		}
	}
	return false
}

// Policy is a token's capability ceiling. The zero value permits everything.
type Policy struct {
	// Operations is the allow-list of verbs. Nil or empty means all operations
	// are allowed; a non-empty list restricts to exactly those.
	Operations []Operation `json:"operations,omitempty"`

	// NetAllowMax is the egress ceiling, and is tri-state:
	//   - nil       → no restriction (any public host the range-filter permits)
	//   - &[]       → no network at all (a request's net_allow must be empty)
	//   - &[hosts…] → a request's net_allow must be a subset of these patterns
	// It is a pointer so "absent" and "present but empty" are distinguishable.
	NetAllowMax *[]string `json:"net_allow_max,omitempty"`

	// NetFullEgress grants the range-based egress modes (full_egress and
	// allowlist_cidr). Default false: a scoped token may NOT broaden egress
	// past its hostname allowlist unless this is set — so a NetAllowMax
	// hostname ceiling can't be bypassed by flipping to full-egress. A nil
	// policy (no token / loopback) permits everything, as elsewhere.
	NetFullEgress bool `json:"net_full_egress,omitempty"`

	// AllowProfiles restricts which rootfs profiles may be launched. Nil/empty
	// means any profile.
	AllowProfiles []string `json:"allow_profiles,omitempty"`

	// Resource ceilings. Zero means "no limit" on that axis; negative is invalid.
	MaxSandboxes int `json:"max_sandboxes,omitempty"` // concurrent live sandboxes for this token
	MaxFork      int `json:"max_fork,omitempty"`      // cap on a single fork's count
	MaxTimeoutS  int `json:"max_timeout_s,omitempty"` // clamp on run/exec command timeout
	MaxVCPUs     int `json:"max_vcpus,omitempty"`     // cap on a create's vCPU count
	MaxMemoryMiB int `json:"max_memory_mib,omitempty"`
}

// Whoami is the GET /whoami response: the effective policy for the presenting
// token. Scoped is false for an unscoped (full-access) key or an
// unauthenticated request; Policy is the enforced policy when scoped. Shared by
// the daemon (writes it) and the client (reads it) so the shape can't drift.
type Whoami struct {
	Scoped bool    `json:"scoped"`
	Policy *Policy `json:"policy,omitempty"`
}

// Parse decodes a policy from JSON, rejecting unknown fields (so a typo'd key is
// an error, not a silently-ignored restriction) and trailing content.
func Parse(data []byte) (Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	if dec.More() {
		return Policy{}, errors.New("policy: parse: unexpected trailing content after JSON object")
	}
	return p, nil
}

// Validate performs static (no daemon needed) semantic checks and returns every
// problem it finds joined together, so an operator sees all of them at once.
// Nil error means the policy is well-formed; it does not check live facts like
// whether a named profile exists (that is the daemon-aware layer's job).
func (p Policy) Validate() error {
	var errs []error

	for _, op := range p.Operations {
		if !isKnownOp(op) {
			errs = append(errs, fmt.Errorf("unknown operation %q (valid: %s)", op, joinOps(KnownOperations())))
		}
	}

	if p.NetAllowMax != nil && len(*p.NetAllowMax) > 0 {
		// Reuse the network layer's validator so policy patterns can never drift
		// from what the allowlist accepts (rejects bare "*", bad labels, etc.).
		if _, err := netallow.New(*p.NetAllowMax); err != nil {
			errs = append(errs, fmt.Errorf("net_allow_max: %w", err))
		}
	}

	for i, prof := range p.AllowProfiles {
		if strings.TrimSpace(prof) == "" {
			errs = append(errs, fmt.Errorf("allow_profiles[%d] is empty", i))
		}
	}

	for _, c := range []struct {
		name string
		val  int
	}{
		{"max_sandboxes", p.MaxSandboxes},
		{"max_fork", p.MaxFork},
		{"max_timeout_s", p.MaxTimeoutS},
		{"max_vcpus", p.MaxVCPUs},
		{"max_memory_mib", p.MaxMemoryMiB},
	} {
		if c.val < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative (got %d)", c.name, c.val))
		}
	}

	return errors.Join(errs...)
}

// ParseAndValidate is the convenience both `policy validate` and `token add`
// use: strict-decode then semantic-validate, so the same checks gate both.
func ParseAndValidate(data []byte) (Policy, error) {
	p, err := Parse(data)
	if err != nil {
		return Policy{}, err
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// --- enforcement checks (pure; the daemon calls these per request) ----------

// Allows reports whether op is permitted. Nil/empty Operations permits all.
func (p Policy) Allows(op Operation) bool {
	if len(p.Operations) == 0 {
		return true
	}
	for _, o := range p.Operations {
		if o == op {
			return true
		}
	}
	return false
}

// CheckProfile rejects a launch whose resolved profile is outside AllowProfiles.
// An empty profile (the daemon's default rootfs) is refused when a profile
// allowlist is set — the policy can't bound something it can't name.
func (p Policy) CheckProfile(profile string) error {
	if len(p.AllowProfiles) == 0 {
		return nil
	}
	for _, a := range p.AllowProfiles {
		if a == profile {
			return nil
		}
	}
	if profile == "" {
		return fmt.Errorf("a profile is required; this token allows: %s", strings.Join(p.AllowProfiles, ", "))
	}
	return fmt.Errorf("profile %q is not allowed by this token (allowed: %s)", profile, strings.Join(p.AllowProfiles, ", "))
}

// CheckNetAllow enforces the egress ceiling: every requested host must appear in
// NetAllowMax (normalized exact match). Nil ceiling permits anything; an empty
// ceiling permits nothing (any requested host is rejected).
func (p Policy) CheckNetAllow(hosts []string) error {
	if p.NetAllowMax == nil {
		return nil
	}
	ceil := make(map[string]struct{}, len(*p.NetAllowMax))
	for _, h := range *p.NetAllowMax {
		ceil[normHost(h)] = struct{}{}
	}
	for _, h := range hosts {
		if _, ok := ceil[normHost(h)]; !ok {
			return fmt.Errorf("host %q is not permitted by this token's network ceiling", h)
		}
	}
	return nil
}

// CheckFullEgress gates the range-based egress modes: full_egress and
// allowlist_cidr may be used only when NetFullEgress is granted. A request that
// asks for either without the grant is rejected, so a hostname NetAllowMax
// ceiling can't be widened by switching to full-egress. Hostname allowlists are
// governed separately by CheckNetAllow.
func (p Policy) CheckFullEgress(wantFullEgress, wantCIDR bool) error {
	if (wantFullEgress || wantCIDR) && !p.NetFullEgress {
		return errors.New("this token is not permitted to broaden egress (full_egress / allowlist_cidr require the net_full_egress grant)")
	}
	return nil
}

// ClampTimeout bounds a command timeout (seconds) by MaxTimeoutS. A zero max
// means no clamp; an unbounded (<=0) or over-limit request is pulled to the max.
func (p Policy) ClampTimeout(sec int) int {
	if p.MaxTimeoutS <= 0 {
		return sec
	}
	if sec <= 0 || sec > p.MaxTimeoutS {
		return p.MaxTimeoutS
	}
	return sec
}

// CheckFork rejects a fork whose count exceeds MaxFork (zero = no cap).
func (p Policy) CheckFork(count int) error {
	if p.MaxFork > 0 && count > p.MaxFork {
		return fmt.Errorf("fork count %d exceeds this token's limit of %d", count, p.MaxFork)
	}
	return nil
}

// CheckCapacity rejects a create/fork that would push this token's live sandbox
// count past MaxSandboxes (zero = no cap). live is the token's current count;
// want is how many the request would add.
func (p Policy) CheckCapacity(live, want int) error {
	if p.MaxSandboxes > 0 && live+want > p.MaxSandboxes {
		return fmt.Errorf("would exceed this token's limit of %d sandboxes (%d already live)", p.MaxSandboxes, live)
	}
	return nil
}

// CheckVCPUs rejects a create requesting more vCPUs than MaxVCPUs (zero = no cap).
func (p Policy) CheckVCPUs(n int) error {
	if p.MaxVCPUs > 0 && n > p.MaxVCPUs {
		return fmt.Errorf("vcpus %d exceeds this token's limit of %d", n, p.MaxVCPUs)
	}
	return nil
}

// CheckMemory rejects a create requesting more memory than MaxMemoryMiB (zero = no cap).
func (p Policy) CheckMemory(mib int) error {
	if p.MaxMemoryMiB > 0 && mib > p.MaxMemoryMiB {
		return fmt.Errorf("memory %d MiB exceeds this token's limit of %d MiB", mib, p.MaxMemoryMiB)
	}
	return nil
}

// normHost normalizes a host pattern for ceiling comparison the same way on both
// sides: lowercase, trimmed, trailing dot stripped. Consistent, so exact-set
// membership is stable regardless of how the caller cased or dotted the name.
func normHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

func joinOps(ops []Operation) string {
	s := make([]string, len(ops))
	for i, o := range ops {
		s[i] = string(o)
	}
	return strings.Join(s, ", ")
}
