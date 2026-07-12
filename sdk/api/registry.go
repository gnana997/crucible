package api

import "time"

// RegistryCredentialRequest is the body of POST /registry/credentials — a
// credential for pulling from a private registry. The secret is write-only: it
// is never returned by any endpoint.
type RegistryCredentialRequest struct {
	// Host is the registry hostname (e.g. ghcr.io, quay.io, index.docker.io, or
	// a self-hosted registry[:port]). Docker Hub aliases canonicalize to
	// index.docker.io.
	Host string `json:"host"`

	// Username may be empty for registries that authenticate on the secret
	// alone (e.g. a token).
	Username string `json:"username,omitempty"`

	// Secret is the registry password or token. Required.
	Secret string `json:"secret"`
}

// RegistryCredential is a stored credential as returned to clients — host and
// username only, never the secret.
type RegistryCredential struct {
	Host      string    `json:"host"`
	Username  string    `json:"username,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RegistryCredentialListResponse is the body of GET /registry/credentials.
type RegistryCredentialListResponse struct {
	Registries []RegistryCredential `json:"registries"`
}
