package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/tokenstore"
)

func netCeil(s []string) *[]string { return &s }

// scopedServer mints a key (scoped if pol != nil) and returns a server whose
// token store holds it, plus the raw key.
func scopedServer(t *testing.T, pol *policy.Policy, ttl time.Duration) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	raw, _, err := tokenstore.Add(path, tokenstore.AddOptions{Name: "agent", Policy: pol, TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return serverWithAuth(t, tokenstore.Open(path)), raw
}

// call sends a request (optional bearer token + body) and returns status + body.
func call(t *testing.T, srv *Server, method, path, token, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func TestOperationGateDeniesUnlistedVerbs(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{Operations: []policy.Operation{policy.OpRead}}, 0)

	if code, body := call(t, srv, "GET", "/sandboxes", tok, ""); code != 200 {
		t.Errorf("read op: GET /sandboxes = %d (%s), want 200", code, body)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/sandboxes"},
		{"DELETE", "/sandboxes/sbx_0000000000000"},
		{"POST", "/snapshots/snap_0000000000000/fork"},
		{"POST", "/sandboxes/sbx_0000000000000/snapshot"},
		// Service mutations are exec-grade: denied to a read-only token.
		{"PUT", "/sandboxes/sbx_0000000000000/service"},
		{"POST", "/sandboxes/sbx_0000000000000/service/start"},
		{"POST", "/sandboxes/sbx_0000000000000/service/stop"},
		{"POST", "/sandboxes/sbx_0000000000000/service/restart"},
	} {
		if code, body := call(t, srv, tc.method, tc.path, tok, "{}"); code != 403 {
			t.Errorf("read-only token: %s %s = %d (%s), want 403", tc.method, tc.path, code, body)
		}
	}
}

func TestReadDeniedWithoutReadOp(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{Operations: []policy.Operation{policy.OpCreate}}, 0)
	if code, _ := call(t, srv, "GET", "/sandboxes", tok, ""); code != 403 {
		t.Errorf("GET without read op = %d, want 403", code)
	}
}

func TestProfileCeiling(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{AllowProfiles: []string{"base"}}, 0)
	if code, body := call(t, srv, "POST", "/sandboxes", tok, `{"profile":"ghost"}`); code != 403 {
		t.Errorf("disallowed profile = %d (%s), want 403", code, body)
	}
}

func TestNetCeilingRejectsOutOfBounds(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{NetAllowMax: netCeil([]string{"pypi.org"})}, 0)
	body := `{"network":{"enabled":true,"allowlist":["evil.com"]}}`
	if code, resp := call(t, srv, "POST", "/sandboxes", tok, body); code != 403 {
		t.Errorf("out-of-ceiling egress = %d (%s), want 403", code, resp)
	}
}

func TestResourceCaps(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{MaxVCPUs: 2, MaxMemoryMiB: 512}, 0)
	if code, _ := call(t, srv, "POST", "/sandboxes", tok, `{"vcpus":4}`); code != 403 {
		t.Errorf("over-vcpu create = %d, want 403", code)
	}
	if code, _ := call(t, srv, "POST", "/sandboxes", tok, `{"memory_mib":1024}`); code != 403 {
		t.Errorf("over-memory create = %d, want 403", code)
	}
}

func TestForkCap(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{MaxFork: 2}, 0)
	if code, body := call(t, srv, "POST", "/snapshots/snap_0000000000000/fork?count=5", tok, ""); code != 403 {
		t.Errorf("fork over cap = %d (%s), want 403", code, body)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{}, -time.Hour) // already expired
	if code, _ := call(t, srv, "GET", "/whoami", tok, ""); code != 401 {
		t.Errorf("expired token = %d, want 401", code)
	}
}

func TestWhoamiScoped(t *testing.T) {
	pol := &policy.Policy{Operations: []policy.Operation{policy.OpRead}, MaxSandboxes: 4}
	srv, tok := scopedServer(t, pol, 0)
	code, body := call(t, srv, "GET", "/whoami", tok, "")
	if code != 200 {
		t.Fatalf("whoami = %d (%s)", code, body)
	}
	var wr struct {
		Scoped bool           `json:"scoped"`
		Policy *policy.Policy `json:"policy"`
	}
	if err := json.Unmarshal([]byte(body), &wr); err != nil {
		t.Fatal(err)
	}
	if !wr.Scoped || wr.Policy == nil || wr.Policy.MaxSandboxes != 4 || !wr.Policy.Allows(policy.OpRead) {
		t.Errorf("whoami body wrong: %s", body)
	}
}

func TestWhoamiUnscopedTokenAndFullAccess(t *testing.T) {
	srv, tok := scopedServer(t, nil, 0) // unscoped key

	// Full access: an unscoped token isn't operation-gated.
	if code, _ := call(t, srv, "GET", "/sandboxes", tok, ""); code != 200 {
		t.Errorf("unscoped GET /sandboxes = %d, want 200", code)
	}
	code, body := call(t, srv, "GET", "/whoami", tok, "")
	if code != 200 || !strings.Contains(body, `"scoped":false`) {
		t.Errorf("unscoped whoami = %d (%s), want scoped:false", code, body)
	}
}

func TestWhoamiNoAuth(t *testing.T) {
	// Auth disabled (empty store): /whoami works and reports scoped:false.
	store := tokenstore.Open(filepath.Join(t.TempDir(), "tokens.json"))
	srv := serverWithAuth(t, store)
	code, body := call(t, srv, "GET", "/whoami", "", "")
	if code != 200 || !strings.Contains(body, `"scoped":false`) {
		t.Errorf("no-auth whoami = %d (%s), want scoped:false", code, body)
	}
}

func TestMaxSandboxesPerToken(t *testing.T) {
	srv, tok := scopedServer(t, &policy.Policy{MaxSandboxes: 1}, 0)
	if code, body := call(t, srv, "POST", "/sandboxes", tok, "{}"); code != 201 {
		t.Fatalf("first create = %d (%s), want 201", code, body)
	}
	if code, body := call(t, srv, "POST", "/sandboxes", tok, "{}"); code != 403 {
		t.Errorf("second create = %d (%s), want 403 (per-token max_sandboxes)", code, body)
	}
}
