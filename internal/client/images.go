package client

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/gnana997/crucible/internal/api"
)

// PullImage pulls and converts a registry image (POST /images). The
// call blocks for the full pull + conversion.
func (c *Client) PullImage(ctx context.Context, ref string) (api.ImageResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/images", api.PullImageRequest{Ref: ref})
	if err != nil {
		return api.ImageResponse{}, err
	}
	return decodeInto[api.ImageResponse](resp)
}

// ImportImage side-loads a docker-save archive from r (POST
// /images/import). tag selects the image in a multi-image archive.
func (c *Client) ImportImage(ctx context.Context, r io.Reader, tag string) (api.ImageResponse, error) {
	path := "/images/import"
	if tag != "" {
		path += "?tag=" + url.QueryEscape(tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, r)
	if err != nil {
		return api.ImageResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return api.ImageResponse{}, err
	}
	return decodeInto[api.ImageResponse](resp)
}

// ListImages returns the converted image cache (GET /images).
func (c *Client) ListImages(ctx context.Context) ([]api.ImageResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.ImageListResponse](resp)
	return out.Images, err
}

// GetImage fetches one image by digest, hex, unique prefix, or ref
// (GET /images/{ref}).
func (c *Client) GetImage(ctx context.Context, ref string) (api.ImageResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref), nil)
	if err != nil {
		return api.ImageResponse{}, err
	}
	return decodeInto[api.ImageResponse](resp)
}

// DeleteImage removes an image (DELETE /images/{ref}).
func (c *Client) DeleteImage(ctx context.Context, ref string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}
