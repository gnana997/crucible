package crucible

import (
	"context"
	"net/http"
	"net/url"

	"github.com/gnana997/crucible/sdk/api"
)

// SetSecret stores or replaces a secret bundle (PUT /secrets/{name}) — a set of
// key→value pairs sealed encrypted at rest. merge=true updates the given keys of
// an existing bundle (keeping others); merge=false replaces the bundle with data.
func (c *Client) SetSecret(ctx context.Context, name string, data map[string]string, merge bool) error {
	resp, err := c.do(ctx, http.MethodPut, "/secrets/"+url.PathEscape(name), api.SecretRequest{Data: data, Merge: merge})
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// ListSecrets returns the names of stored secret bundles (GET /secrets) — never
// their contents.
func (c *Client) ListSecrets(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/secrets", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.SecretListResponse](resp)
	return out.Secrets, err
}

// SecretKeys returns one bundle's key NAMES (GET /secrets/{name}) — never the
// values.
func (c *Client) SecretKeys(ctx context.Context, name string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.SecretKeysResponse](resp)
	return out.Keys, err
}

// DeleteSecret removes a secret bundle (DELETE /secrets/{name}).
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}
