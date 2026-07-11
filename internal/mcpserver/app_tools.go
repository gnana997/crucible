package mcpserver

import (
	"context"
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
	Name       string   `json:"name" jsonschema:"app name (a DNS label, e.g. web); unique per daemon"`
	Image      string   `json:"image" jsonschema:"OCI image the app boots from, e.g. \"nginx:alpine\"; the daemon pulls+converts on a store miss"`
	Pull       string   `json:"pull,omitempty" jsonschema:"image pull policy: missing (default), always, or never"`
	Publish    []string `json:"publish,omitempty" jsonschema:"host port mappings [HOST_IP:]HOST:GUEST[/tcp]"`
	PublishAll bool     `json:"publish_all,omitempty" jsonschema:"publish every port the image EXPOSEs (guest N → host N); explicit publish entries win"`
	Env        []string `json:"env,omitempty" jsonschema:"environment variables as KEY=VALUE strings for the app's entrypoint"`
	Restart    string   `json:"restart,omitempty" jsonschema:"instance restart policy: always (default), on-failure, or never"`
	VCPUs      int      `json:"vcpus,omitempty" jsonschema:"vCPUs; omit for the daemon default"`
	MemoryMiB  int      `json:"memory_mib,omitempty" jsonschema:"memory in MiB; omit for the daemon default"`
	HealthType string   `json:"health_type,omitempty" jsonschema:"health check type: http or tcp (omit for none)"`
	HealthPort int      `json:"health_port,omitempty" jsonschema:"guest port the health check probes"`
	HealthPath string   `json:"health_path,omitempty" jsonschema:"http health check path (default /)"`
	Stopped    bool     `json:"stopped,omitempty" jsonschema:"create the app without starting an instance"`
}

type appNameInput struct {
	Name string `json:"name" jsonschema:"the app's name"`
}

type appOutput struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DesiredState string `json:"desired_state"`
	Phase        string `json:"phase,omitempty"`
	Health       string `json:"health,omitempty"`
	Restarts     int    `json:"restarts,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

type appListOutput struct {
	Apps []appOutput `json:"apps"`
}

func toAppOutput(a api.AppResponse) appOutput {
	out := appOutput{ID: a.ID, Name: a.Name, DesiredState: a.DesiredState}
	if a.Status != nil {
		out.Phase, out.Health, out.Restarts = a.Status.Phase, a.Status.Health, a.Status.Restarts
		out.InstanceID, out.LastError = a.Status.InstanceID, a.Status.LastError
	}
	return out
}

func (h *handlers) createApp(ctx context.Context, _ *mcp.CallToolRequest, in createAppInput) (*mcp.CallToolResult, appOutput, error) {
	if in.Name == "" || in.Image == "" {
		return nil, appOutput{}, fmt.Errorf("name and image are required")
	}
	restart := in.Restart
	if restart == "" {
		restart = wire.RestartAlways
	}
	envMap, err := api.ParseEnv(in.Env)
	if err != nil {
		return nil, appOutput{}, err
	}
	spec := api.AppSpec{
		Name:       in.Name,
		Image:      &api.ImageRef{OCI: in.Image},
		Pull:       in.Pull,
		VCPUs:      in.VCPUs,
		MemoryMiB:  in.MemoryMiB,
		Env:        envMap,
		PublishAll: in.PublishAll,
		Restart:    wire.RestartPolicy{Policy: restart},
	}
	for _, p := range in.Publish {
		pm, err := api.ParsePublish(p)
		if err != nil {
			return nil, appOutput{}, err
		}
		spec.Publish = append(spec.Publish, pm)
	}
	if in.HealthType != "" {
		spec.Health = &api.HealthCheck{Type: in.HealthType, Port: in.HealthPort, Path: in.HealthPath}
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
