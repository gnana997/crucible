package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Tests that exercise the NetworkRequest.validate path end-to-end
// via POST /sandboxes. These are HTTP-level tests because the
// validate method is unexported; going through the handler also
// confirms the wire contract and the handler's error mapping.

func postCreateNetworkBody(t *testing.T, body map[string]any) *http.Response {
	t.Helper()
	ts, _ := newTestServer(t)
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestNetworkDisabledWithAllowlistIsRejected(t *testing.T) {
	// enabled=false + non-empty allowlist is a user bug — the
	// allowlist is meaningless without enabling the NIC.
	resp := postCreateNetworkBody(t, map[string]any{
		"network": map[string]any{
			"enabled":   false,
			"allowlist": []string{"pypi.org"},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "allowlist set but") {
		t.Errorf("error = %q, want 'allowlist set but ...'", e.Error)
	}
}

func TestNetworkEnabledWithEmptyAllowlistIsRejected(t *testing.T) {
	// enabled=true + empty allowlist would be "full internet",
	// which violates our default-deny ethos. v0.1 rejects
	// explicitly rather than silently allowing everything.
	resp := postCreateNetworkBody(t, map[string]any{
		"network": map[string]any{
			"enabled": true,
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "non-empty allowlist") {
		t.Errorf("error = %q, want 'non-empty allowlist ...'", e.Error)
	}
}

func TestNetworkEnabledBadPatternRejected(t *testing.T) {
	// Bare "*" is refused at allowlist parse time per docs/network.md.
	resp := postCreateNetworkBody(t, map[string]any{
		"network": map[string]any{
			"enabled":   true,
			"allowlist": []string{"*"},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "bare wildcard") {
		t.Errorf("error = %q, want to mention bare wildcard", e.Error)
	}
}

func TestNetworkEnabledWithoutProvisionerErrors(t *testing.T) {
	// The test server's sandbox.Manager has no Network provisioner
	// configured (newTestServer doesn't wire one). A valid network
	// request should therefore fail at Create time with a 500
	// saying network isn't available — not a 400, because the
	// request itself is well-formed; the daemon just can't honor
	// it.
	resp := postCreateNetworkBody(t, map[string]any{
		"network": map[string]any{
			"enabled":   true,
			"allowlist": []string{"pypi.org"},
		},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (daemon lacks provisioner)", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "network") {
		t.Errorf("error = %q, want to mention network", e.Error)
	}
}

func TestNetworkAbsentWorksUnchanged(t *testing.T) {
	// Sanity: a request without the network field still creates a
	// sandbox successfully. Guards against a regression where we
	// accidentally require network config for everything.
	resp := postCreateNetworkBody(t, map[string]any{
		"vcpus": 1,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	var got sandboxResponse
	decodeJSON(t, resp, &got)
	if got.Network != nil {
		t.Errorf("Network = %+v, want nil for no-network request", got.Network)
	}
}
