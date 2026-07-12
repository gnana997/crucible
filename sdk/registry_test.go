package crucible

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
)

func TestRegistryLoginLogoutList(t *testing.T) {
	var hits []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/registry/credentials":
			var req api.RegistryCredentialRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode login body: %v", err)
			}
			if req.Host != "ghcr.io" || req.Username != "alice" || req.Secret != "tok" {
				t.Errorf("login body = %+v", req)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/registry/credentials":
			_ = json.NewEncoder(w).Encode(api.RegistryCredentialListResponse{
				Registries: []api.RegistryCredential{{Host: "ghcr.io", Username: "alice"}},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()

	if err := c.RegistryLogin(ctx, "ghcr.io", "alice", "tok"); err != nil {
		t.Fatalf("RegistryLogin: %v", err)
	}
	creds, err := c.ListRegistryCredentials(ctx)
	if err != nil || len(creds) != 1 || creds[0].Host != "ghcr.io" {
		t.Fatalf("ListRegistryCredentials: %+v err=%v", creds, err)
	}
	if err := c.RegistryLogout(ctx, "ghcr.io"); err != nil {
		t.Fatalf("RegistryLogout: %v", err)
	}
	if last := hits[len(hits)-1]; last != "DELETE /registry/credentials/ghcr.io" {
		t.Errorf("logout hit %q, want DELETE /registry/credentials/ghcr.io", last)
	}
}
