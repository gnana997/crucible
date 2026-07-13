package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/gnana997/crucible/sdk"
)

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
