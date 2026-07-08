//go:build embedagent

package agentbin

import _ "embed"

// Binary is the static crucible-agent, embedded at build time. Present
// only under the embedagent tag; make build copies the binary into
// place first.
//
//go:embed crucible-agent
var Binary []byte
