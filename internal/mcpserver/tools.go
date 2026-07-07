package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
)

// --- tool input types: the stable contract an agent's tools/list sees -------
//
// Field docs come from the `jsonschema` struct tag; the SDK infers each tool's
// input schema from these types.

// noInput is the input for tools that take no parameters.
type noInput struct{}

type runInput struct {
	Profile  string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the daemon's default rootfs"`
	Command  []string `json:"command" jsonschema:"command argv to run, e.g. [\"python\",\"-c\",\"print(1)\"]"`
	Env      []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings"`
	TimeoutS int      `json:"timeout_s,omitempty" jsonschema:"wall-clock timeout in seconds"`
	NetAllow []string `json:"net_allow,omitempty" jsonschema:"hostnames the sandbox may reach; empty means no network"`
}

type createSandboxInput struct {
	Profile   string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the daemon's default rootfs"`
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

// --- tool output types ------------------------------------------------------

type execOutput struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	OomKilled  bool   `json:"oom_killed"`
	DurationMs int64  `json:"duration_ms"`
	Signal     string `json:"signal,omitempty"`
	Error      string `json:"error,omitempty"`
}

type networkOutput struct {
	Enabled   bool     `json:"enabled"`
	GuestIP   string   `json:"guest_ip,omitempty"`
	Allowlist []string `json:"allowlist,omitempty"`
}

type sandboxOutput struct {
	ID        string         `json:"id"`
	Profile   string         `json:"profile,omitempty"`
	VCPUs     int            `json:"vcpus"`
	MemoryMiB int            `json:"memory_mib"`
	Workdir   string         `json:"workdir,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Network   *networkOutput `json:"network,omitempty"`
}

type sandboxListOutput struct {
	Sandboxes []sandboxOutput `json:"sandboxes"`
}

type snapshotOutput struct {
	SnapshotID string    `json:"snapshot_id"`
	SourceID   string    `json:"source_id"`
	VCPUs      int       `json:"vcpus"`
	MemoryMiB  int       `json:"memory_mib"`
	CreatedAt  time.Time `json:"created_at"`
}

type snapshotListOutput struct {
	Snapshots []snapshotOutput `json:"snapshots"`
}

type forkOutput struct {
	SandboxIDs []string `json:"sandbox_ids"`
}

type deletedOutput struct {
	Deleted string `json:"deleted"`
}

type profilesOutput struct {
	Profiles []string `json:"profiles"`
}

func toSandboxOutput(s api.SandboxResponse) sandboxOutput {
	out := sandboxOutput{
		ID: s.ID, Profile: s.Profile, VCPUs: s.VCPUs,
		MemoryMiB: s.MemoryMiB, Workdir: s.Workdir, CreatedAt: s.CreatedAt,
	}
	if s.Network != nil {
		out.Network = &networkOutput{
			Enabled: s.Network.Enabled, GuestIP: s.Network.GuestIP, Allowlist: s.Network.Allowlist,
		}
	}
	return out
}

func toSnapshotOutput(s api.SnapshotResponse) snapshotOutput {
	return snapshotOutput{
		SnapshotID: s.ID, SourceID: s.SourceID,
		VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB, CreatedAt: s.CreatedAt,
	}
}

func toExecOutput(r agentwire.ExecResult, stdout, stderr string) execOutput {
	return execOutput{
		ExitCode: r.ExitCode, Stdout: stdout, Stderr: stderr,
		TimedOut: r.TimedOut, OomKilled: r.OomKilled,
		DurationMs: r.DurationMs, Signal: r.Signal, Error: r.Error,
	}
}

// envMap turns KEY=VALUE strings into the map ExecRequest wants.
func envMap(kv []string) (map[string]string, error) {
	if len(kv) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			return nil, fmt.Errorf("env %q must be KEY=VALUE", e)
		}
		m[k] = v
	}
	return m, nil
}

// registerTools wires the crucible catalog to handlers backed by the daemon
// client. Each handler is a thin translation: MCP input → guardrail checks →
// one (or, for run, a few) internal/client calls → MCP output. Business logic
// lives in the daemon, so a tool and the equivalent CLI command cannot drift.
//
// A tool disabled by --tools/--deny-tools is never registered, so it never
// appears in tools/list.
func registerTools(srv *mcp.Server, cfg Config) {
	h := &handlers{cfg: cfg}
	add := func(name, desc string, register func(name, desc string)) {
		if cfg.toolEnabled(name) && policyPermitsTool(cfg.Policy, name) {
			register(name, desc)
		}
	}

	add("run", "Create a sandbox, run one command, return its output, then delete the sandbox. The 80% case for running untrusted code.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.run) })
	add("create_sandbox", "Create a persistent sandbox and return its id. Drive it with exec/snapshot/fork, then delete_sandbox.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.createSandbox) })
	add("exec", "Run a command in an existing sandbox and return its captured output.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.exec) })
	add("snapshot", "Snapshot a sandbox's warm state so it can be forked.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.snapshot) })
	add("fork", "Create N independent, clone-safe sandboxes from a snapshot.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.fork) })
	add("list_sandboxes", "List the live sandboxes.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listSandboxes) })
	add("inspect_sandbox", "Return full detail for one sandbox.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.inspectSandbox) })
	add("delete_sandbox", "Destroy a sandbox.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.deleteSandbox) })
	add("list_snapshots", "List the snapshots.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listSnapshots) })
	add("delete_snapshot", "Delete a snapshot.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.deleteSnapshot) })
	add("list_profiles", "List the rootfs profiles the daemon offers.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listProfiles) })
}

// handlers carries the operator policy (including the daemon client) the tool
// handlers enforce and translate calls into.
type handlers struct {
	cfg Config
}

func (h *handlers) run(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, execOutput, error) {
	if len(in.Command) == 0 {
		return nil, execOutput{}, errors.New("command must not be empty")
	}
	env, err := envMap(in.Env)
	if err != nil {
		return nil, execOutput{}, err
	}
	profile, err := h.cfg.resolveProfile(in.Profile)
	if err != nil {
		return nil, execOutput{}, err
	}
	if err := h.cfg.checkNetAllow(in.NetAllow); err != nil {
		return nil, execOutput{}, err
	}
	if err := h.cfg.checkCapacity(ctx, 1); err != nil {
		return nil, execOutput{}, err
	}
	timeout := h.cfg.clampTimeout(in.TimeoutS)

	req := api.CreateSandboxRequest{Profile: profile, TimeoutSec: timeout}
	if len(in.NetAllow) > 0 {
		req.Network = &api.NetworkRequest{Enabled: true, Allowlist: in.NetAllow}
	}
	sb, err := h.cfg.Client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, execOutput{}, err
	}
	// Always delete — background ctx so cleanup runs even if ctx was cancelled.
	defer func() { _ = h.cfg.Client.DeleteSandbox(context.Background(), sb.ID) }()

	var stdout, stderr bytes.Buffer
	res, err := h.cfg.Client.Exec(ctx, sb.ID, agentwire.ExecRequest{
		Cmd: in.Command, Env: env, TimeoutSec: timeout,
	}, &stdout, &stderr)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, toExecOutput(res, stdout.String(), stderr.String()), nil
}

func (h *handlers) createSandbox(ctx context.Context, _ *mcp.CallToolRequest, in createSandboxInput) (*mcp.CallToolResult, sandboxOutput, error) {
	profile, err := h.cfg.resolveProfile(in.Profile)
	if err != nil {
		return nil, sandboxOutput{}, err
	}
	if err := h.cfg.checkNetAllow(in.NetAllow); err != nil {
		return nil, sandboxOutput{}, err
	}
	if err := h.cfg.checkCapacity(ctx, 1); err != nil {
		return nil, sandboxOutput{}, err
	}
	req := api.CreateSandboxRequest{
		Profile: profile, VCPUs: in.Vcpus, MemoryMiB: in.MemoryMib, TimeoutSec: in.TimeoutS,
	}
	if len(in.NetAllow) > 0 {
		req.Network = &api.NetworkRequest{Enabled: true, Allowlist: in.NetAllow}
	}
	sb, err := h.cfg.Client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, sandboxOutput{}, err
	}
	return nil, toSandboxOutput(sb), nil
}

func (h *handlers) exec(ctx context.Context, _ *mcp.CallToolRequest, in execInput) (*mcp.CallToolResult, execOutput, error) {
	if in.SandboxID == "" {
		return nil, execOutput{}, errors.New("sandbox_id is required")
	}
	if len(in.Command) == 0 {
		return nil, execOutput{}, errors.New("command must not be empty")
	}
	env, err := envMap(in.Env)
	if err != nil {
		return nil, execOutput{}, err
	}
	var stdout, stderr bytes.Buffer
	res, err := h.cfg.Client.Exec(ctx, in.SandboxID, agentwire.ExecRequest{
		Cmd: in.Command, Cwd: in.Cwd, Env: env, TimeoutSec: h.cfg.clampTimeout(in.TimeoutS),
	}, &stdout, &stderr)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, toExecOutput(res, stdout.String(), stderr.String()), nil
}

func (h *handlers) snapshot(ctx context.Context, _ *mcp.CallToolRequest, in sandboxIDInput) (*mcp.CallToolResult, snapshotOutput, error) {
	if in.SandboxID == "" {
		return nil, snapshotOutput{}, errors.New("sandbox_id is required")
	}
	snap, err := h.cfg.Client.Snapshot(ctx, in.SandboxID)
	if err != nil {
		return nil, snapshotOutput{}, err
	}
	return nil, toSnapshotOutput(snap), nil
}

func (h *handlers) fork(ctx context.Context, _ *mcp.CallToolRequest, in forkInput) (*mcp.CallToolResult, forkOutput, error) {
	if in.SnapshotID == "" {
		return nil, forkOutput{}, errors.New("snapshot_id is required")
	}
	count, err := h.cfg.checkFork(in.Count)
	if err != nil {
		return nil, forkOutput{}, err
	}
	if err := h.cfg.checkCapacity(ctx, count); err != nil {
		return nil, forkOutput{}, err
	}
	forks, err := h.cfg.Client.Fork(ctx, in.SnapshotID, count)
	if err != nil {
		return nil, forkOutput{}, err
	}
	ids := make([]string, len(forks))
	for i, f := range forks {
		ids[i] = f.ID
	}
	return nil, forkOutput{SandboxIDs: ids}, nil
}

func (h *handlers) listSandboxes(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, sandboxListOutput, error) {
	sbs, err := h.cfg.Client.ListSandboxes(ctx)
	if err != nil {
		return nil, sandboxListOutput{}, err
	}
	out := sandboxListOutput{Sandboxes: make([]sandboxOutput, len(sbs))}
	for i, s := range sbs {
		out.Sandboxes[i] = toSandboxOutput(s)
	}
	return nil, out, nil
}

func (h *handlers) inspectSandbox(ctx context.Context, _ *mcp.CallToolRequest, in sandboxIDInput) (*mcp.CallToolResult, sandboxOutput, error) {
	if in.SandboxID == "" {
		return nil, sandboxOutput{}, errors.New("sandbox_id is required")
	}
	sb, err := h.cfg.Client.GetSandbox(ctx, in.SandboxID)
	if err != nil {
		return nil, sandboxOutput{}, err
	}
	return nil, toSandboxOutput(sb), nil
}

func (h *handlers) deleteSandbox(ctx context.Context, _ *mcp.CallToolRequest, in sandboxIDInput) (*mcp.CallToolResult, deletedOutput, error) {
	if in.SandboxID == "" {
		return nil, deletedOutput{}, errors.New("sandbox_id is required")
	}
	if err := h.cfg.Client.DeleteSandbox(ctx, in.SandboxID); err != nil {
		return nil, deletedOutput{}, err
	}
	return nil, deletedOutput{Deleted: in.SandboxID}, nil
}

func (h *handlers) listSnapshots(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, snapshotListOutput, error) {
	snaps, err := h.cfg.Client.ListSnapshots(ctx)
	if err != nil {
		return nil, snapshotListOutput{}, err
	}
	out := snapshotListOutput{Snapshots: make([]snapshotOutput, len(snaps))}
	for i, s := range snaps {
		out.Snapshots[i] = toSnapshotOutput(s)
	}
	return nil, out, nil
}

func (h *handlers) deleteSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in snapshotIDInput) (*mcp.CallToolResult, deletedOutput, error) {
	if in.SnapshotID == "" {
		return nil, deletedOutput{}, errors.New("snapshot_id is required")
	}
	if err := h.cfg.Client.DeleteSnapshot(ctx, in.SnapshotID); err != nil {
		return nil, deletedOutput{}, err
	}
	return nil, deletedOutput{Deleted: in.SnapshotID}, nil
}

func (h *handlers) listProfiles(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, profilesOutput, error) {
	profs, err := h.cfg.Client.ListProfiles(ctx)
	if err != nil {
		return nil, profilesOutput{}, err
	}
	return nil, profilesOutput{Profiles: profs}, nil
}
