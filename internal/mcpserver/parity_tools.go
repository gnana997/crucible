package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
)

// --- volume management (volume_create / list_volumes / delete_volume) --------

type volumeToolOutput struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	AttachedTo string `json:"attached_to,omitempty"`
	HostID     string `json:"host_id,omitempty"`
}

type volumeListToolOutput struct {
	Volumes []volumeToolOutput `json:"volumes"`
}

type createVolumeInput struct {
	Name      string `json:"name" jsonschema:"the durable volume name ([a-z0-9][a-z0-9-]*)"`
	SizeBytes int64  `json:"size_bytes,omitempty" jsonschema:"size in bytes (0 = the daemon default)"`
}

func (h *handlers) createVolume(ctx context.Context, _ *mcp.CallToolRequest, in createVolumeInput) (*mcp.CallToolResult, volumeToolOutput, error) {
	if in.Name == "" {
		return nil, volumeToolOutput{}, errors.New("name is required")
	}
	v, err := h.cfg.Client.CreateVolume(ctx, api.CreateVolumeRequest{Name: in.Name, SizeBytes: in.SizeBytes})
	if err != nil {
		return nil, volumeToolOutput{}, err
	}
	return nil, volumeToolOutput{Name: v.Name, SizeBytes: v.SizeBytes, AttachedTo: v.AttachedTo, HostID: v.HostID}, nil
}

func (h *handlers) listVolumes(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, volumeListToolOutput, error) {
	vols, err := h.cfg.Client.ListVolumes(ctx)
	if err != nil {
		return nil, volumeListToolOutput{}, err
	}
	out := volumeListToolOutput{Volumes: make([]volumeToolOutput, len(vols.Items))}
	for i, v := range vols.Items {
		out.Volumes[i] = volumeToolOutput{Name: v.Name, SizeBytes: v.SizeBytes, AttachedTo: v.AttachedTo, HostID: v.HostID}
	}
	return nil, out, nil
}

type volumeNameInput struct {
	Name string `json:"name" jsonschema:"the volume to delete (refused while attached to a live sandbox)"`
}

func (h *handlers) deleteVolume(ctx context.Context, _ *mcp.CallToolRequest, in volumeNameInput) (*mcp.CallToolResult, deletedOutput, error) {
	if in.Name == "" {
		return nil, deletedOutput{}, errors.New("name is required")
	}
	if err := h.cfg.Client.DeleteVolume(ctx, in.Name); err != nil {
		return nil, deletedOutput{}, err
	}
	return nil, deletedOutput{Deleted: in.Name}, nil
}

// --- volume backups (volume_backup / volume_restore) ------------------------

type backupToolOutput struct {
	ID           string `json:"id"`
	SourceVolume string `json:"source_volume"`
	SizeBytes    int64  `json:"size_bytes"`
	Consistency  string `json:"consistency,omitempty"`
}

type backupVolumeInput struct {
	Name string `json:"name" jsonschema:"the volume to back up (must be detached or slept, not attached to a running sandbox)"`
}

func (h *handlers) backupVolume(ctx context.Context, _ *mcp.CallToolRequest, in backupVolumeInput) (*mcp.CallToolResult, backupToolOutput, error) {
	if in.Name == "" {
		return nil, backupToolOutput{}, errors.New("name is required")
	}
	b, err := h.cfg.Client.BackupVolume(ctx, in.Name)
	if err != nil {
		return nil, backupToolOutput{}, err
	}
	return nil, backupToolOutput{ID: b.ID, SourceVolume: b.SourceVolume, SizeBytes: b.SizeBytes, Consistency: b.Consistency}, nil
}

type restoreVolumeInput struct {
	From string `json:"from" jsonschema:"the backup id to restore"`
	To   string `json:"to" jsonschema:"name of the new volume to create (refused if it already exists)"`
}

func (h *handlers) restoreVolume(ctx context.Context, _ *mcp.CallToolRequest, in restoreVolumeInput) (*mcp.CallToolResult, volumeToolOutput, error) {
	if in.From == "" || in.To == "" {
		return nil, volumeToolOutput{}, errors.New("from and to are required")
	}
	v, err := h.cfg.Client.RestoreBackup(ctx, in.From, in.To)
	if err != nil {
		return nil, volumeToolOutput{}, err
	}
	return nil, volumeToolOutput{Name: v.Name, SizeBytes: v.SizeBytes, AttachedTo: v.AttachedTo, HostID: v.HostID}, nil
}

// --- image management (list_images / delete_image) --------------------------

type imageOutput struct {
	Digest     string   `json:"digest"`
	Ref        string   `json:"ref,omitempty"`
	SizeBytes  int64    `json:"size_bytes"`
	Entrypoint []string `json:"entrypoint,omitempty"`
}

type imageListOutput struct {
	Images []imageOutput `json:"images"`
}

func (h *handlers) listImages(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, imageListOutput, error) {
	imgs, err := h.cfg.Client.ListImages(ctx)
	if err != nil {
		return nil, imageListOutput{}, err
	}
	out := imageListOutput{Images: make([]imageOutput, len(imgs.Items))}
	for i, im := range imgs.Items {
		out.Images[i] = imageOutput{Digest: im.Digest, Ref: im.SourceRef, SizeBytes: im.SizeBytes, Entrypoint: im.Entrypoint}
	}
	return nil, out, nil
}

type imageRefInput struct {
	Ref string `json:"ref" jsonschema:"the image to delete: full digest, hex prefix, or source ref"`
}

func (h *handlers) deleteImage(ctx context.Context, _ *mcp.CallToolRequest, in imageRefInput) (*mcp.CallToolResult, deletedOutput, error) {
	if in.Ref == "" {
		return nil, deletedOutput{}, errors.New("ref is required")
	}
	if err := h.cfg.Client.DeleteImage(ctx, in.Ref); err != nil {
		return nil, deletedOutput{}, err
	}
	return nil, deletedOutput{Deleted: in.Ref}, nil
}

// --- packet capture (capture) ----------------------------------------------

type captureInput struct {
	SandboxID  string `json:"sandbox_id,omitempty" jsonschema:"the sandbox/instance id to capture; provide this or app"`
	App        string `json:"app,omitempty" jsonschema:"an app name, captured on its current instance (alternative to sandbox_id)"`
	Filter     string `json:"filter,omitempty" jsonschema:"BPF filter expression, e.g. 'tcp port 8080'"`
	MaxSeconds int    `json:"max_seconds,omitempty" jsonschema:"capture duration in seconds (default 15, max 120)"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"stop after this many bytes"`
}

type captureOutput struct {
	Path      string `json:"path"` // local pcap file on the host running `mcp serve`
	Bytes     int64  `json:"bytes"`
	SandboxID string `json:"sandbox_id"`
}

// capture writes a bounded pcap of a sandbox's (or app's current instance)
// traffic to a local temp file and returns its path — a file result, not a raw
// binary stream, which suits an MCP tool. Requires the `capture` scoped op.
// Duration is clamped so an agent can't hold a capture open indefinitely.
func (h *handlers) capture(ctx context.Context, _ *mcp.CallToolRequest, in captureInput) (*mcp.CallToolResult, captureOutput, error) {
	id := in.SandboxID
	if id == "" && in.App != "" {
		app, err := h.cfg.Client.GetApp(ctx, in.App)
		if err != nil {
			return nil, captureOutput{}, err
		}
		if app.Status == nil || app.Status.InstanceID == "" {
			return nil, captureOutput{}, fmt.Errorf("app %q has no running instance", in.App)
		}
		id = app.Status.InstanceID
	}
	if id == "" {
		return nil, captureOutput{}, errors.New("sandbox_id or app is required")
	}

	secs := in.MaxSeconds
	if secs <= 0 {
		secs = 15
	}
	if secs > 120 {
		secs = 120
	}

	f, err := os.CreateTemp("", "crucible-capture-*.pcap")
	if err != nil {
		return nil, captureOutput{}, err
	}
	name := f.Name()
	cerr := h.cfg.Client.Capture(ctx, id, client.CaptureOptions{
		Filter:     in.Filter,
		MaxSeconds: secs,
		MaxBytes:   int64(in.MaxBytes),
	}, f)
	_ = f.Close()
	if cerr != nil {
		_ = os.Remove(name)
		return nil, captureOutput{}, cerr
	}
	var size int64
	if fi, serr := os.Stat(name); serr == nil {
		size = fi.Size()
	}
	return nil, captureOutput{Path: name, Bytes: size, SandboxID: id}, nil
}
