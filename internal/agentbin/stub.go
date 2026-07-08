//go:build !embedagent

package agentbin

// Binary is nil in non-embed builds; the daemon falls back to
// --agent-bin. See package doc.
var Binary []byte
