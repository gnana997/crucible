package crucible

import (
	"context"
	"net/http"
	"net/url"

	"github.com/gnana997/crucible/sdk/api"
)

// CreateVolume provisions a persistent volume (POST /volumes). A zero SizeBytes
// uses the daemon default. Errors 409 if the name already exists.
func (c *Client) CreateVolume(ctx context.Context, req api.CreateVolumeRequest) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodPost, "/volumes", req)
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}

// ListVolumes returns every volume with its live attachment (GET /volumes).
func (c *Client) ListVolumes(ctx context.Context) (Page[api.Volume], error) {
	resp, err := c.do(ctx, http.MethodGet, "/volumes", nil)
	if err != nil {
		return Page[api.Volume]{}, err
	}
	out, err := decodeInto[api.VolumeListResponse](resp)
	return Page[api.Volume]{Items: out.Volumes}, err
}

// GetVolume fetches one volume by name (GET /volumes/{name}).
func (c *Client) GetVolume(ctx context.Context, name string) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}

// DeleteVolume removes a volume and its backing file (DELETE /volumes/{name}).
// Errors 409 if the volume is attached to a live sandbox.
func (c *Client) DeleteVolume(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}
