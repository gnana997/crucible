package mcpserver

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// --- tool input types: the stable contract an agent's tools/list sees -------
//
// Field docs come from the `jsonschema` struct tag; the SDK infers each tool's
// input schema from these types.

// noInput is the input for tools that take no parameters.
type noInput struct{}

type runInput struct {
	Profile       string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the daemon's default rootfs. Mutually exclusive with image."`
	Image         string   `json:"image,omitempty" jsonschema:"OCI image to run the command inside, e.g. \"python:3.12-slim\" or a converted digest; the daemon pulls+converts on a store miss. Mutually exclusive with profile."`
	Pull          string   `json:"pull,omitempty" jsonschema:"image pull policy when image is set: missing (default), always, or never"`
	Command       []string `json:"command" jsonschema:"command argv to run, e.g. [\"python\",\"-c\",\"print(1)\"]"`
	Env           []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings"`
	DiskMib       int      `json:"disk_mib,omitempty" jsonschema:"grow the writable rootfs to at least this many MiB (default: image/profile headroom)"`
	TimeoutS      int      `json:"timeout_s,omitempty" jsonschema:"wall-clock timeout in seconds"`
	NetAllow      []string `json:"net_allow,omitempty" jsonschema:"hostnames the sandbox may reach; empty means no network"`
	NetAllowCIDR  []string `json:"net_allow_cidr,omitempty" jsonschema:"public IPv4 CIDRs the sandbox may reach directly, e.g. [\"203.0.113.0/24\"]"`
	NetFullEgress bool     `json:"net_full_egress,omitempty" jsonschema:"allow egress to ANY public host (metadata/link-local/RFC1918 still blocked). Subject to the server's --net-allow-max ceiling."`
}

type createSandboxInput struct {
	Profile       string   `json:"profile,omitempty" jsonschema:"rootfs profile to launch; omit to use the daemon's default rootfs. Mutually exclusive with image."`
	Image         string   `json:"image,omitempty" jsonschema:"OCI image to boot instead of a profile, e.g. \"nginx:alpine\" or a converted digest; the daemon pulls+converts on a store miss and runs its entrypoint. Mutually exclusive with profile."`
	Pull          string   `json:"pull,omitempty" jsonschema:"image pull policy when image is set: missing (default), always, or never"`
	Vcpus         int      `json:"vcpus,omitempty" jsonschema:"number of vCPUs"`
	MemoryMib     int      `json:"memory_mib,omitempty" jsonschema:"memory in MiB"`
	DiskMib       int      `json:"disk_mib,omitempty" jsonschema:"grow the writable rootfs to at least this many MiB (default: image/profile headroom)"`
	TimeoutS      int      `json:"timeout_s,omitempty" jsonschema:"sandbox idle timeout in seconds"`
	NetAllow      []string `json:"net_allow,omitempty" jsonschema:"hostnames the sandbox may reach; empty means no network"`
	NetAllowCIDR  []string `json:"net_allow_cidr,omitempty" jsonschema:"public IPv4 CIDRs the sandbox may reach directly, e.g. [\"203.0.113.0/24\"]"`
	NetFullEgress bool     `json:"net_full_egress,omitempty" jsonschema:"allow egress to ANY public host (metadata/link-local/RFC1918 still blocked). Subject to the server's --net-allow-max ceiling."`
	Publish       []string `json:"publish,omitempty" jsonschema:"host port publishes so a guest service is reachable from the host, e.g. [\"8080:80\"] or [\"127.0.0.1:8080:80\"]"`
	PublishAll    bool     `json:"publish_all,omitempty" jsonschema:"publish every port the image EXPOSEs (guest N → host N); explicit publish entries win. Image mode only."`
}

type logsInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"id of the sandbox whose logs to read"`
	Source    string `json:"source,omitempty" jsonschema:"filter: service (entrypoint output), exec (command activity), or all (default)"`
	Since     int64  `json:"since,omitempty" jsonschema:"byte cursor from a previous call's next_offset to continue from; omit to read the recent tail"`
}

type stopSandboxInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"id of the sandbox to gracefully stop (StopSignal + grace, then SIGKILL). The sandbox remains; use delete_sandbox to remove it."`
	GraceS    int    `json:"grace_s,omitempty" jsonschema:"override the image's stop grace in seconds before SIGKILL"`
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

type publishOutput struct {
	HostIP    string `json:"host_ip,omitempty"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Protocol  string `json:"protocol,omitempty"`
}

type sandboxOutput struct {
	ID        string          `json:"id"`
	Profile   string          `json:"profile,omitempty"`
	VCPUs     int             `json:"vcpus"`
	MemoryMiB int             `json:"memory_mib"`
	Workdir   string          `json:"workdir,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Network   *networkOutput  `json:"network,omitempty"`
	Published []publishOutput `json:"published,omitempty"`
}

type logRecordOutput struct {
	TimeMs int64  `json:"time_ms"`
	Source string `json:"source"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type logsOutput struct {
	Records    []logRecordOutput `json:"records"`
	NextOffset int64             `json:"next_offset"`
}

type stoppedOutput struct {
	Stopped string `json:"stopped"`
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
	for _, pm := range s.Published {
		out.Published = append(out.Published, publishOutput{
			HostIP: pm.HostIP, HostPort: pm.HostPort, GuestPort: pm.GuestPort, Protocol: pm.Protocol,
		})
	}
	return out
}

func toSnapshotOutput(s api.SnapshotResponse) snapshotOutput {
	return snapshotOutput{
		SnapshotID: s.ID, SourceID: s.SourceID,
		VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB, CreatedAt: s.CreatedAt,
	}
}

func toExecOutput(r wire.ExecResult, stdout, stderr string) execOutput {
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
// one (or, for run, a few) SDK client calls → MCP output. Business logic
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
	add("write_files", "Write files into a sandbox by content (no image build, no Dockerfile). Paths are absolute inside the guest; parents are created and existing files overwritten.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.writeFiles) })
	add("read_file", "Read the content of a single file from a sandbox (e.g. a test report or generated file). Returns bounded content; binary is returned base64-encoded.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.readFile) })
	add("logs", "Read a sandbox's durable logs (entrypoint output and/or exec activity). Logs survive the sandbox, so a crashed workload can still be inspected.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.logs) })
	add("stop_sandbox", "Gracefully stop a sandbox's entrypoint (StopSignal + grace, then SIGKILL). The sandbox remains; use delete_sandbox to remove it.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.stopSandbox) })
	add("snapshot", "Snapshot a sandbox's warm state so it can be forked.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.snapshot) })
	add("fork", "Create N independent, clone-safe sandboxes from a snapshot.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.fork) })
	add("create_app", "Create a durable app: a named workload the daemon keeps a healthy instance of, restarting it on failure and re-creating it after a daemon restart. Publish a port to reach it.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.createApp) })
	add("update_app", "Update a durable app: replace its spec (same fields as create_app; name immutable) and redeploy — the old instance is destroyed and a fresh one booted from the new spec. Desired running/stopped is retained (the stopped field is ignored).",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.updateApp) })
	add("list_apps", "List durable apps with their phase, health, and restart count.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listApps) })
	add("get_app", "Get one app's desired state and observed status (instance id, phase, health, restarts).",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.getApp) })
	add("delete_app", "Delete a durable app and tear down its instance.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.deleteApp) })
	add("app_sleep", "Put a durable app to sleep: snapshot its instance and free its RAM (scale-to-zero). The app stays addressable and wakes on demand.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.sleepApp) })
	add("app_wake", "Wake a slept durable app: restore its instance in place (same IP), reseeding its RNG and correcting its clock.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.wakeApp) })
	add("app_exec", "Run a command in a durable app's CURRENT instance and return its captured output. The app is addressed by name and resolved to its live instance per call, so this stays correct across a self-heal or rolling update.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.appExec) })
	add("app_logs", "Read a durable app's current-instance logs (entrypoint output and/or exec activity), addressed by name.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.appLogs) })
	add("app_domain_add", "Attach a custom domain (FQDN, globally unique) to a durable app. The ingress proxy then routes that host to the app and, in terminate mode, obtains a TLS certificate for it.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.appDomainAdd) })
	add("app_domain_rm", "Detach a custom domain from a durable app.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.appDomainRm) })
	add("app_domain_ls", "List the custom domains attached to a durable app.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.appDomainLs) })
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
	add("list_images", "List the converted OCI images in the daemon's cache (digest, ref, size).",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listImages) })
	add("delete_image", "Delete a converted image from the cache by digest, hex prefix, or ref.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.deleteImage) })
	add("capture", "Capture a sandbox's (or an app's current instance's) network traffic to a local pcap file and return its path. Bounded by max_seconds/max_bytes; requires the 'capture' scoped op. Open the file in Wireshark.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.capture) })
	add("volume_create", "Create a persistent volume (a durable block device, formatted ext4). Attach it to a sandbox with --volume NAME:/path; data survives the sandbox.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.createVolume) })
	add("list_volumes", "List persistent volumes (name, size, which sandbox has each attached).",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.listVolumes) })
	add("delete_volume", "Delete a persistent volume and its data by name. Refused while the volume is attached to a live sandbox.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.deleteVolume) })
	add("volume_backup", "Back up a volume: a consistent point-in-time copy, restorable to a new volume. Refused while the volume is attached to a running sandbox (sleep the app first).",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.backupVolume) })
	add("volume_restore", "Restore a backup into a NEW volume (from = backup id, to = new volume name). Never overwrites an existing volume.",
		func(n, d string) { mcp.AddTool(srv, &mcp.Tool{Name: n, Description: d}, h.restoreVolume) })
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
	if in.Image != "" && in.Profile != "" {
		return nil, execOutput{}, errors.New("image and profile are mutually exclusive")
	}
	env, err := envMap(in.Env)
	if err != nil {
		return nil, execOutput{}, err
	}
	if err := h.cfg.checkNetAllow(in.NetAllow); err != nil {
		return nil, execOutput{}, err
	}
	if err := h.cfg.checkFullEgress(in.NetFullEgress, len(in.NetAllowCIDR) > 0); err != nil {
		return nil, execOutput{}, err
	}
	if err := h.cfg.checkCapacity(ctx, 1); err != nil {
		return nil, execOutput{}, err
	}
	timeout := h.cfg.clampTimeout(in.TimeoutS)

	req := api.CreateSandboxRequest{TimeoutSec: timeout, DiskBytes: mibToBytes(in.DiskMib)}
	if in.Image != "" {
		req.Image = &api.ImageRef{OCI: in.Image}
		req.Pull = in.Pull
	} else {
		profile, err := h.cfg.resolveProfile(in.Profile)
		if err != nil {
			return nil, execOutput{}, err
		}
		req.Profile = profile
	}
	req.Network = mcpNetwork(in.NetAllow, in.NetAllowCIDR, in.NetFullEgress)
	sb, err := h.cfg.Client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, execOutput{}, err
	}
	// Always delete — background ctx so cleanup runs even if ctx was cancelled.
	defer func() { _ = h.cfg.Client.DeleteSandbox(context.Background(), sb.ID) }()

	var stdout, stderr bytes.Buffer
	res, err := h.cfg.Client.Exec(ctx, sb.ID, wire.ExecRequest{
		Cmd: in.Command, Env: env, TimeoutSec: timeout,
	}, &stdout, &stderr)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, toExecOutput(res, stdout.String(), stderr.String()), nil
}

func (h *handlers) createSandbox(ctx context.Context, _ *mcp.CallToolRequest, in createSandboxInput) (*mcp.CallToolResult, sandboxOutput, error) {
	if in.Image != "" && in.Profile != "" {
		return nil, sandboxOutput{}, errors.New("image and profile are mutually exclusive")
	}
	if err := h.cfg.checkNetAllow(in.NetAllow); err != nil {
		return nil, sandboxOutput{}, err
	}
	if err := h.cfg.checkFullEgress(in.NetFullEgress, len(in.NetAllowCIDR) > 0); err != nil {
		return nil, sandboxOutput{}, err
	}
	if err := h.cfg.checkCapacity(ctx, 1); err != nil {
		return nil, sandboxOutput{}, err
	}
	req := api.CreateSandboxRequest{
		VCPUs: in.Vcpus, MemoryMiB: in.MemoryMib, TimeoutSec: in.TimeoutS, DiskBytes: mibToBytes(in.DiskMib),
	}
	if in.Image != "" {
		req.Image = &api.ImageRef{OCI: in.Image}
		req.Pull = in.Pull
	} else {
		profile, err := h.cfg.resolveProfile(in.Profile)
		if err != nil {
			return nil, sandboxOutput{}, err
		}
		req.Profile = profile
	}
	req.Network = mcpNetwork(in.NetAllow, in.NetAllowCIDR, in.NetFullEgress)
	for _, p := range in.Publish {
		pm, err := api.ParsePublish(p)
		if err != nil {
			return nil, sandboxOutput{}, err
		}
		req.Publish = append(req.Publish, pm)
	}
	req.PublishAll = in.PublishAll
	sb, err := h.cfg.Client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, sandboxOutput{}, err
	}
	return nil, toSandboxOutput(sb), nil
}

// mibToBytes converts a MiB count to bytes; 0 stays 0 (unset).
func mibToBytes(mib int) int64 { return int64(mib) << 20 }

func (h *handlers) logs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
	if in.SandboxID == "" {
		return nil, logsOutput{}, errors.New("sandbox_id is required")
	}
	switch in.Source {
	case "", "all", "service", "exec":
	default:
		return nil, logsOutput{}, errors.New("source must be service, exec, or all")
	}
	// Default (0/omitted) tails the recent log; the daemon tails on since < 0.
	since := in.Since
	if since == 0 {
		since = -1
	}
	resp, err := h.cfg.Client.Logs(ctx, in.SandboxID, since, in.Source)
	if err != nil {
		return nil, logsOutput{}, err
	}
	out := logsOutput{NextOffset: resp.NextOffset}
	for _, r := range resp.Records {
		out.Records = append(out.Records, logRecordOutput{
			TimeMs: r.TimeMs, Source: r.Source, Stream: r.Stream, Text: r.Text,
		})
	}
	return nil, out, nil
}

func (h *handlers) stopSandbox(ctx context.Context, _ *mcp.CallToolRequest, in stopSandboxInput) (*mcp.CallToolResult, stoppedOutput, error) {
	if in.SandboxID == "" {
		return nil, stoppedOutput{}, errors.New("sandbox_id is required")
	}
	if _, err := h.cfg.Client.StopService(ctx, in.SandboxID, in.GraceS); err != nil {
		return nil, stoppedOutput{}, err
	}
	return nil, stoppedOutput{Stopped: in.SandboxID}, nil
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
	res, err := h.cfg.Client.Exec(ctx, in.SandboxID, wire.ExecRequest{
		Cmd: in.Command, Cwd: in.Cwd, Env: env, TimeoutSec: h.cfg.clampTimeout(in.TimeoutS),
	}, &stdout, &stderr)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, toExecOutput(res, stdout.String(), stderr.String()), nil
}

// mcpMaxReadBytes caps read_file when the caller omits max_bytes — modest so a
// file's content doesn't blow up the agent's context window.
const mcpMaxReadBytes = 1 << 20

type writeFileEntry struct {
	Path    string `json:"path" jsonschema:"absolute path in the guest, e.g. /work/main.py"`
	Content string `json:"content" jsonschema:"the file's content (UTF-8 text)"`
	Mode    string `json:"mode,omitempty" jsonschema:"octal permissions like \"0644\" (default 0644)"`
}

type writeFilesInput struct {
	SandboxID string           `json:"sandbox_id" jsonschema:"id of the sandbox to write into"`
	Files     []writeFileEntry `json:"files" jsonschema:"files to create/overwrite in the guest"`
}

type writeFilesOutput struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (h *handlers) writeFiles(ctx context.Context, _ *mcp.CallToolRequest, in writeFilesInput) (*mcp.CallToolResult, writeFilesOutput, error) {
	if in.SandboxID == "" {
		return nil, writeFilesOutput{}, errors.New("sandbox_id is required")
	}
	if len(in.Files) == 0 {
		return nil, writeFilesOutput{}, errors.New("files must not be empty")
	}
	// Build a tar whose entries are the requested absolute paths (leading slash
	// stripped) and push it beneath the guest root, reusing the cp machinery.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range in.Files {
		if f.Path == "" || !path.IsAbs(f.Path) {
			return nil, writeFilesOutput{}, fmt.Errorf("file path must be absolute: %q", f.Path)
		}
		mode := int64(0o644)
		if f.Mode != "" {
			m, err := strconv.ParseInt(f.Mode, 8, 32)
			if err != nil {
				return nil, writeFilesOutput{}, fmt.Errorf("invalid mode %q: %w", f.Mode, err)
			}
			mode = m
		}
		hdr := &tar.Header{
			Name:     strings.TrimPrefix(path.Clean(f.Path), "/"),
			Typeflag: tar.TypeReg,
			Mode:     mode,
			Size:     int64(len(f.Content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, writeFilesOutput{}, err
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			return nil, writeFilesOutput{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, writeFilesOutput{}, err
	}

	res, err := h.cfg.Client.CopyTo(ctx, in.SandboxID, "/", &buf)
	if err != nil {
		return nil, writeFilesOutput{}, err
	}
	return nil, writeFilesOutput{Files: res.Files, Bytes: res.Bytes}, nil
}

type readFileInput struct {
	SandboxID string `json:"sandbox_id" jsonschema:"id of the sandbox to read from"`
	Path      string `json:"path" jsonschema:"absolute path of the file in the guest"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"cap the read in bytes (default 1 MiB)"`
}

type readFileOutput struct {
	Path      string `json:"path"`
	Content   string `json:"content" jsonschema:"file content; base64-encoded when the file is binary (see base64)"`
	Bytes     int    `json:"bytes"`
	Base64    bool   `json:"base64" jsonschema:"true when content is base64-encoded binary"`
	Truncated bool   `json:"truncated" jsonschema:"true when the file was longer than the read cap"`
}

func (h *handlers) readFile(ctx context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, readFileOutput, error) {
	if in.SandboxID == "" {
		return nil, readFileOutput{}, errors.New("sandbox_id is required")
	}
	if in.Path == "" {
		return nil, readFileOutput{}, errors.New("path is required")
	}
	max := in.MaxBytes
	if max <= 0 {
		max = mcpMaxReadBytes
	}
	// Ask the daemon for one extra byte so we can report truncation accurately.
	data, err := h.cfg.Client.ReadFile(ctx, in.SandboxID, in.Path, max+1)
	if err != nil {
		return nil, readFileOutput{}, err
	}
	truncated := len(data) > max
	if truncated {
		data = data[:max]
	}
	out := readFileOutput{Path: in.Path, Bytes: len(data), Truncated: truncated}
	if utf8.Valid(data) {
		out.Content = string(data)
	} else {
		out.Content = base64.StdEncoding.EncodeToString(data)
		out.Base64 = true
	}
	return nil, out, nil
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
	out := sandboxListOutput{Sandboxes: make([]sandboxOutput, len(sbs.Items))}
	for i, s := range sbs.Items {
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
	out := snapshotListOutput{Snapshots: make([]snapshotOutput, len(snaps.Items))}
	for i, s := range snaps.Items {
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
