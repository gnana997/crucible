package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// App tools expose durable apps (v0.4): named workloads the daemon keeps a
// healthy instance of and re-creates after a restart. The operator's
// guardrails (max-sandboxes, profile/image pins, token policy) apply to an
// app's instance exactly as they do to a create.

type createAppInput struct {
	Name          string   `json:"name" jsonschema:"app name (a DNS label, e.g. web); unique per daemon"`
	Image         string   `json:"image" jsonschema:"OCI image the app boots from, e.g. \"nginx:alpine\"; the daemon pulls+converts on a store miss"`
	Pull          string   `json:"pull,omitempty" jsonschema:"image pull policy: missing (default), always, or never"`
	Publish       []string `json:"publish,omitempty" jsonschema:"host port mappings [HOST_IP:]HOST:GUEST[/tcp]"`
	PublishAll    bool     `json:"publish_all,omitempty" jsonschema:"publish every port the image EXPOSEs (guest N → host N); explicit publish entries win"`
	Env           []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings for the app's entrypoint"`
	Restart       string   `json:"restart,omitempty" jsonschema:"instance restart policy: always (default), on-failure, or never"`
	VCPUs         int      `json:"vcpus,omitempty" jsonschema:"vCPUs; omit for the daemon default"`
	MemoryMiB     int      `json:"memory_mib,omitempty" jsonschema:"memory in MiB; omit for the daemon default"`
	Port          int      `json:"port,omitempty" jsonschema:"guest port the ingress proxy forwards to when routing this app by name (omit to default from a single published port)"`
	HealthType    string   `json:"health_type,omitempty" jsonschema:"health check type: http, tcp, or exec (omit for none)"`
	HealthPort    int      `json:"health_port,omitempty" jsonschema:"guest port an http/tcp health check probes"`
	HealthPath    string   `json:"health_path,omitempty" jsonschema:"http health check path (default /)"`
	HealthCmd     []string `json:"health_cmd,omitempty" jsonschema:"exec health check: command argv run in the guest, exit 0 = healthy (used when health_type is exec)"`
	NetAllow      []string `json:"net_allow,omitempty" jsonschema:"egress hostname allowlist for the app (repeatable); empty means no network"`
	NetAllowCIDR  []string `json:"net_allow_cidr,omitempty" jsonschema:"public IPv4 CIDRs the app may reach directly, e.g. [\"203.0.113.0/24\"]"`
	NetFullEgress bool     `json:"net_full_egress,omitempty" jsonschema:"allow the app egress to ANY public host (metadata/link-local/RFC1918 still blocked). Subject to the server's --net-allow-max ceiling."`
	Stopped       bool     `json:"stopped,omitempty" jsonschema:"create the app without starting an instance"`
}

type appNameInput struct {
	Name string `json:"name" jsonschema:"the app's name"`
}

type appOutput struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	DesiredState       string `json:"desired_state"`
	Generation         uint64 `json:"generation,omitempty"`
	Phase              string `json:"phase,omitempty"`
	Health             string `json:"health,omitempty"`
	Restarts           int    `json:"restarts,omitempty"`
	InstanceID         string `json:"instance_id,omitempty"`
	InstanceGeneration uint64 `json:"instance_generation,omitempty"`
	LastError          string `json:"last_error,omitempty"`
}

type appListOutput struct {
	Apps []appOutput `json:"apps"`
}

func toAppOutput(a api.AppResponse) appOutput {
	out := appOutput{ID: a.ID, Name: a.Name, DesiredState: a.DesiredState, Generation: a.Generation}
	if a.Status != nil {
		out.Phase, out.Health, out.Restarts = a.Status.Phase, a.Status.Health, a.Status.Restarts
		out.InstanceID, out.LastError = a.Status.InstanceID, a.Status.LastError
		out.InstanceGeneration = a.Status.InstanceGeneration
	}
	return out
}

// app_exec / app_logs operate a deployed app BY NAME: the daemon resolves the
// name to the app's current instance per request, so they stay correct across a
// self-heal or rolling update (an agent never has to track the instance id).

type appExecInput struct {
	AppName  string   `json:"app_name" jsonschema:"name of the app to run in (resolved to its current instance)"`
	Command  []string `json:"command" jsonschema:"command argv to run"`
	Cwd      string   `json:"cwd,omitempty" jsonschema:"working directory inside the guest"`
	Env      []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings"`
	TimeoutS int      `json:"timeout_s,omitempty" jsonschema:"wall-clock timeout in seconds"`
}

type appLogsInput struct {
	AppName string `json:"app_name" jsonschema:"name of the app whose current-instance logs to read"`
	Source  string `json:"source,omitempty" jsonschema:"filter: service (entrypoint output), exec (command activity), or all (default)"`
	Since   int64  `json:"since,omitempty" jsonschema:"byte cursor from a previous call's next_offset to continue from; omit to read the recent tail"`
}

func (h *handlers) appExec(ctx context.Context, _ *mcp.CallToolRequest, in appExecInput) (*mcp.CallToolResult, execOutput, error) {
	if in.AppName == "" {
		return nil, execOutput{}, errors.New("app_name is required")
	}
	if len(in.Command) == 0 {
		return nil, execOutput{}, errors.New("command must not be empty")
	}
	env, err := envMap(in.Env)
	if err != nil {
		return nil, execOutput{}, err
	}
	var stdout, stderr bytes.Buffer
	res, err := h.cfg.Client.AppExec(ctx, in.AppName, wire.ExecRequest{
		Cmd: in.Command, Cwd: in.Cwd, Env: env, TimeoutSec: h.cfg.clampTimeout(in.TimeoutS),
	}, &stdout, &stderr)
	if err != nil {
		return nil, execOutput{}, err
	}
	return nil, toExecOutput(res, stdout.String(), stderr.String()), nil
}

func (h *handlers) appLogs(ctx context.Context, _ *mcp.CallToolRequest, in appLogsInput) (*mcp.CallToolResult, logsOutput, error) {
	if in.AppName == "" {
		return nil, logsOutput{}, errors.New("app_name is required")
	}
	switch in.Source {
	case "", "all", "service", "exec":
	default:
		return nil, logsOutput{}, errors.New("source must be service, exec, or all")
	}
	since := in.Since
	if since == 0 {
		since = -1
	}
	resp, err := h.cfg.Client.AppLogs(ctx, in.AppName, since, in.Source)
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

// appSpecFrom builds a validated AppSpec from the tool input, applying the
// operator's egress guardrails. Shared by create_app and update_app.
func (h *handlers) appSpecFrom(in createAppInput) (api.AppSpec, error) {
	if in.Name == "" || in.Image == "" {
		return api.AppSpec{}, fmt.Errorf("name and image are required")
	}
	if err := h.cfg.checkNetAllow(in.NetAllow); err != nil {
		return api.AppSpec{}, err
	}
	if err := h.cfg.checkFullEgress(in.NetFullEgress, len(in.NetAllowCIDR) > 0); err != nil {
		return api.AppSpec{}, err
	}
	envMap, err := api.ParseEnv(in.Env)
	if err != nil {
		return api.AppSpec{}, err
	}
	restart := in.Restart
	if restart == "" {
		restart = wire.RestartAlways
	}
	spec := api.AppSpec{
		Name:       in.Name,
		Image:      &api.ImageRef{OCI: in.Image},
		Pull:       in.Pull,
		VCPUs:      in.VCPUs,
		MemoryMiB:  in.MemoryMiB,
		Env:        envMap,
		Port:       in.Port,
		PublishAll: in.PublishAll,
		Network:    mcpNetwork(in.NetAllow, in.NetAllowCIDR, in.NetFullEgress),
		Restart:    wire.RestartPolicy{Policy: restart},
	}
	for _, p := range in.Publish {
		pm, perr := api.ParsePublish(p)
		if perr != nil {
			return api.AppSpec{}, perr
		}
		spec.Publish = append(spec.Publish, pm)
	}
	if in.HealthType != "" {
		spec.Health = &api.HealthCheck{Type: in.HealthType, Port: in.HealthPort, Path: in.HealthPath, Cmd: in.HealthCmd}
	}
	return spec, nil
}

func (h *handlers) createApp(ctx context.Context, _ *mcp.CallToolRequest, in createAppInput) (*mcp.CallToolResult, appOutput, error) {
	spec, err := h.appSpecFrom(in)
	if err != nil {
		return nil, appOutput{}, err
	}
	desired := "running"
	if in.Stopped {
		desired = "stopped"
	}
	resp, err := h.cfg.Client.CreateApp(ctx, api.CreateAppRequest{AppSpec: spec, DesiredState: desired})
	if err != nil {
		return nil, appOutput{}, err
	}
	return nil, toAppOutput(resp), nil
}

// updateApp replaces an app's spec and redeploys it (destroy the old instance,
// boot a fresh one from the new spec). The name is immutable and desired
// running/stopped is retained (the "stopped" input field is ignored here).
func (h *handlers) updateApp(ctx context.Context, _ *mcp.CallToolRequest, in createAppInput) (*mcp.CallToolResult, appOutput, error) {
	spec, err := h.appSpecFrom(in)
	if err != nil {
		return nil, appOutput{}, err
	}
	resp, err := h.cfg.Client.UpdateApp(ctx, in.Name, spec)
	if err != nil {
		return nil, appOutput{}, err
	}
	return nil, toAppOutput(resp), nil
}

func (h *handlers) listApps(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, appListOutput, error) {
	page, err := h.cfg.Client.ListApps(ctx)
	if err != nil {
		return nil, appListOutput{}, err
	}
	out := appListOutput{Apps: make([]appOutput, len(page.Items))}
	for i, a := range page.Items {
		out.Apps[i] = toAppOutput(a)
	}
	return nil, out, nil
}

func (h *handlers) getApp(ctx context.Context, _ *mcp.CallToolRequest, in appNameInput) (*mcp.CallToolResult, appOutput, error) {
	if in.Name == "" {
		return nil, appOutput{}, fmt.Errorf("name is required")
	}
	resp, err := h.cfg.Client.GetApp(ctx, in.Name)
	if err != nil {
		return nil, appOutput{}, err
	}
	return nil, toAppOutput(resp), nil
}

func (h *handlers) deleteApp(ctx context.Context, _ *mcp.CallToolRequest, in appNameInput) (*mcp.CallToolResult, deletedOutput, error) {
	if in.Name == "" {
		return nil, deletedOutput{}, fmt.Errorf("name is required")
	}
	if err := h.cfg.Client.DeleteApp(ctx, in.Name); err != nil {
		return nil, deletedOutput{}, err
	}
	return nil, deletedOutput{Deleted: in.Name}, nil
}
