package network

// The egress allowlist matcher is pure hostname logic with no Linux
// dependencies, so it lives in the leaf package internal/netallow — that lets
// cross-platform clients (e.g. `internal/policy`, which validates policies on
// macOS/Windows) reuse the exact same validator without dragging in this
// package's netlink/nft/dhcp machinery. We re-export it here as a type alias
// so every existing network.Allowlist / network.New caller keeps working.

import "github.com/gnana997/crucible/internal/netallow"

// Allowlist is an alias for netallow.Allowlist. Being an alias (not a new
// type) keeps *network.Allowlist and *netallow.Allowlist interchangeable, so
// the daemon's type assertions on the api boundary still hold.
type Allowlist = netallow.Allowlist

// New parses hostname patterns into an Allowlist. See netallow.New.
func New(patterns []string) (*Allowlist, error) { return netallow.New(patterns) }
