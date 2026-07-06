// Package mcpserver exposes crucible as MCP tools over stdio. It is a thin
// consumer of internal/client: every tool call becomes one typed client call
// against the daemon's REST API, so an MCP tool and the equivalent CLI command
// hit the identical code path and cannot drift. The server owns no sandbox
// state — all state lives in the daemon.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gnana997/crucible/internal/client"
	"github.com/gnana997/crucible/internal/version"
)

// Config is the operator-set policy for one `mcp serve` session. The security
// guardrails (resource caps, surface reduction, net-allow ceiling) land in a
// later phase; for now the server just needs a client to bridge to the daemon.
type Config struct {
	// Client talks to the daemon this server fronts.
	Client *client.Client
}

// New builds the MCP server and registers the crucible tool catalog. It is the
// seam the tests drive over an in-memory transport; Serve wraps it for stdio.
func New(cfg Config) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "crucible",
		Title:   "crucible sandbox runtime",
		Version: version.String(),
	}, nil)
	registerTools(srv, cfg)
	return srv
}

// Serve runs the server over stdio until the agent closes stdin or ctx is
// cancelled.
func Serve(ctx context.Context, cfg Config) error {
	return New(cfg).Run(ctx, &mcp.StdioTransport{})
}
