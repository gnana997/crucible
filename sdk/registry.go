package crucible

import (
	"context"
	"net/http"
	"net/url"

	"github.com/gnana997/crucible/sdk/api"
)

// RegistryLogin stores (or replaces) the credential the daemon uses to pull
// from a private registry (POST /registry/credentials). The secret is
// write-only — no endpoint ever returns it. username may be empty for
// registries that authenticate on the secret alone.
func (c *Client) RegistryLogin(ctx context.Context, host, username, secret string) error {
	resp, err := c.do(ctx, http.MethodPost, "/registry/credentials",
		api.RegistryCredentialRequest{Host: host, Username: username, Secret: secret})
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// RegistryLogout removes the stored credential for a registry host
// (DELETE /registry/credentials/{host}).
func (c *Client) RegistryLogout(ctx context.Context, host string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/registry/credentials/"+url.PathEscape(host), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// ListRegistryCredentials lists stored credentials as host + username only —
// never the secret (GET /registry/credentials).
func (c *Client) ListRegistryCredentials(ctx context.Context) ([]api.RegistryCredential, error) {
	resp, err := c.do(ctx, http.MethodGet, "/registry/credentials", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.RegistryCredentialListResponse](resp)
	return out.Registries, err
}
