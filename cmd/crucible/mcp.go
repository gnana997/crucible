package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/internal/mcpserver"
)

// newMcpCmd is the MCP entry point. `crucible mcp serve` runs a stdio MCP
// server that bridges an agent to the daemon at --addr (authenticated with
// --token). Because the server is just a client, "stdio" is not "local only":
// point --addr at a remote daemon and this same local subprocess bridges to it.
//
// The flags below are the operator's policy: the agent runs strictly within
// them and cannot widen them (see internal/mcpserver guardrails).
func newMcpCmd(o *globalOpts) *cobra.Command {
	cfg := mcpserver.Config{}
	var maxTimeout time.Duration
	cmd := &cobra.Command{Use: "mcp", Short: "Expose crucible to MCP agents"}
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run an MCP server over stdio, bridging to the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// stdin EOF (the agent disconnecting) is the normal stop; also
			// honor Ctrl-C / SIGTERM so a foreground run exits cleanly.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			cfg.Client = o.client()
			cfg.MaxTimeout = maxTimeout
			// The agent closing stdin or a signal is the normal way an stdio
			// session ends — exit 0, not an error.
			if err := mcpserver.Serve(ctx, cfg); !isCleanShutdown(err) {
				return err
			}
			return nil
		},
	}
	f := serve.Flags()
	f.StringVar(&cfg.DefaultProfile, "default-profile", "", "rootfs profile used when a tool omits one")
	f.StringSliceVar(&cfg.AllowProfiles, "allow-profiles", nil, "restrict which rootfs profiles the tools may launch (comma-separated)")
	f.StringSliceVar(&cfg.NetAllowMax, "net-allow-max", nil, "ceiling for agent-chosen egress: net_allow must be a subset (comma-separated; unset = any public host)")
	f.IntVar(&cfg.MaxSandboxes, "max-sandboxes", 8, "max concurrent live sandboxes (0 = unlimited)")
	f.IntVar(&cfg.MaxFork, "max-fork", 8, "cap on the fork tool's count (0 = unlimited)")
	f.DurationVar(&maxTimeout, "max-timeout", 300*time.Second, "clamp on every run/exec command timeout (0 = no clamp)")
	f.StringSliceVar(&cfg.Tools, "tools", nil, "expose only these tools (comma-separated; default all)")
	f.StringSliceVar(&cfg.DenyTools, "deny-tools", nil, "hide these tools (comma-separated)")

	cmd.AddCommand(serve)
	return cmd
}

// isCleanShutdown reports whether err is a normal end of an stdio session:
// stdin EOF, a cancelling signal, or the SDK's connection-closing sentinel.
// The SDK wraps its "server is closing" jsonrpc2 error with the underlying EOF
// via %v (not %w) and keeps the sentinel in an internal package, so it is
// neither unwrappable to io.EOF nor importable — a string check is the only
// seam the SDK leaves us for it.
func isCleanShutdown(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(err.Error(), "server is closing") ||
		strings.Contains(err.Error(), "client is closing")
}
