package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errNotImplemented is returned by the scaffold handlers until the tools phase
// wires each one to its internal/client call.
var errNotImplemented = errors.New("not implemented yet")

// --- tool input types: the stable contract an agent's tools/list sees -------
//
// Field docs come from the `jsonschema` struct tag; the SDK infers each tool's
// input schema from these types.

// noInput is the input for tools that take no parameters.
type noInput struct{}

type runInput struct {
	Profile  string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the server's default profile"`
	Command  []string `json:"command" jsonschema:"command argv to run, e.g. [\"python\",\"-c\",\"print(1)\"]"`
	Env      []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings"`
	TimeoutS int      `json:"timeout_s,omitempty" jsonschema:"wall-clock timeout in seconds"`
	NetAllow []string `json:"net_allow,omitempty" jsonschema:"hostnames the sandbox may reach; empty means no network"`
}

type createSandboxInput struct {
	Profile   string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the server's default profile"`
	Vcpus     int      `json:"vcpus,omitempty" jsonschema:"number of vCPUs"`
	MemoryMib int      `json:"memory_mib,omitempty" jsonschema:"memory in MiB"`
	TimeoutS  int      `json:"timeout_s,omitempty" jsonschema:"sandbox idle timeout in seconds"`
	NetAllow  []string `json:"net_allow,omitempty" jsonschema:"hostnames the sandbox may reach; empty means no network"`
}

type execInput struct {
	SandboxID string   `json:"sandbox_id" jsonschema:"id of the sandbox to run in"`
	Command   []string `json:"command" jsonschema:"command argv to run"`
	Cwd       string   `json:"cwd,omitempty" jsonschema:"working directory inside the guest"`
	Env       []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings"`
	TimeoutS  int      `json:"timeout_s,omitempty" jsonschema:"wall-clock timeout in seconds"`
}

type sandboxIDInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"id of the sandbox"`
}

type snapshotIDInput struct {
	SnapshotID string `json:"snapshot_id" jsonschema:"id of the snapshot"`
}

type forkInput struct {
	SnapshotID string `json:"snapshot_id" jsonschema:"id of the snapshot to fork from"`
	Count      int    `json:"count,omitempty" jsonschema:"number of independent forks to create (default 1)"`
}

// registerTools advertises the full crucible catalog. The handler bodies are
// stubbed in this scaffolding phase; the schemas registered here are the stable
// surface agents discover via tools/list.
func registerTools(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "run",
		Description: "Create a sandbox, run one command, return its output, then delete the sandbox. The 80% case for running untrusted code.",
	}, stub[runInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_sandbox",
		Description: "Create a persistent sandbox and return its id. Drive it with exec/snapshot/fork, then delete_sandbox.",
	}, stub[createSandboxInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exec",
		Description: "Run a command in an existing sandbox and return its captured output.",
	}, stub[execInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "snapshot",
		Description: "Snapshot a sandbox's warm state so it can be forked.",
	}, stub[sandboxIDInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fork",
		Description: "Create N independent, clone-safe sandboxes from a snapshot.",
	}, stub[forkInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_sandboxes",
		Description: "List the live sandboxes.",
	}, stub[noInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "inspect_sandbox",
		Description: "Return full detail for one sandbox.",
	}, stub[sandboxIDInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_sandbox",
		Description: "Destroy a sandbox.",
	}, stub[sandboxIDInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_snapshots",
		Description: "List the snapshots.",
	}, stub[noInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_snapshot",
		Description: "Delete a snapshot.",
	}, stub[snapshotIDInput](cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_profiles",
		Description: "List the rootfs profiles the daemon offers.",
	}, stub[noInput](cfg))
}

// stub is the placeholder handler used while scaffolding. The tools phase
// replaces each registration with a real handler that calls cfg.Client.
func stub[In any](cfg Config) mcp.ToolHandlerFor[In, any] {
	_ = cfg
	return func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
		return nil, nil, errNotImplemented
	}
}
