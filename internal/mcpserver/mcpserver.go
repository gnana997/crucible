// Package mcpserver exposes crucible as MCP tools over stdio. It is a thin
// consumer of internal/client: every tool call becomes one typed client call
// against the daemon's REST API, so an MCP tool and the equivalent CLI command
// hit the identical code path and cannot drift. The server owns no sandbox
// state — all state lives in the daemon.
package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gnana997/crucible/internal/client"
	"github.com/gnana997/crucible/internal/version"
)

// Config is the operator-set policy for one `mcp serve` session. The operator
// fixes this policy at launch; the agent operates strictly within it and can
// never widen it — an LLM can't rewrite the server's flags.
type Config struct {
	// Client talks to the daemon this server fronts.
	Client *client.Client

	// DefaultProfile is the rootfs used when a tool omits `profile` (required
	// for run/create to work without an explicit profile). Empty falls through
	// to the daemon's own default rootfs.
	DefaultProfile string

	// AllowProfiles, when non-empty, restricts which rootfs profiles the tools
	// may launch. A launch resolving to a profile outside the list is refused.
	AllowProfiles []string

	// NetAllowMax, when non-empty, is the ceiling on agent-chosen egress: every
	// host in a tool's net_allow must appear here (exact match). Empty means the
	// agent may allowlist any public host (the daemon's range-filter still
	// blocks internal/metadata addresses regardless).
	NetAllowMax []string

	// MaxSandboxes caps concurrent live sandboxes on the daemon (best-effort,
	// checked before each create). Zero means unlimited.
	MaxSandboxes int

	// MaxFork caps the fork tool's count. Zero means unlimited.
	MaxFork int

	// MaxTimeout clamps every run/exec command timeout so nothing runs forever.
	// Zero means no clamp.
	MaxTimeout time.Duration

	// Tools, when non-empty, is the allowlist of tool names to expose. DenyTools
	// is subtracted afterward. A tool that is filtered out is never registered,
	// so it does not appear in tools/list.
	Tools     []string
	DenyTools []string
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
