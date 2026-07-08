// Package agentbin optionally carries the static crucible-agent binary
// embedded into the crucible daemon, so converted OCI images boot an
// agent that is guaranteed to match the daemon version (no stale-rootfs
// class for images).
//
// The embed is behind the `embedagent` build tag. `make build` copies
// the freshly-built agent to internal/agentbin/crucible-agent and
// builds with -tags embedagent; plain `go build ./...` / `go test ./...`
// use the stub (Binary == nil) and need no binary present, so the tree
// always compiles. When Binary is nil the daemon falls back to
// --agent-bin.
package agentbin
