package mcpserver

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func TestToolMirrorByPolicy(t *testing.T) {
	// A token permitted only to read + exec: exec and the list/inspect tools
	// show; run (needs create+exec+delete), create_sandbox, snapshot, fork, and
	// the delete tools are hidden.
	pol := &policy.Policy{Operations: []policy.Operation{policy.OpRead, policy.OpExec}}
	cs := connect(t, Config{Policy: pol})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	shown := []string{"exec", "app_exec", "app_sleep", "app_wake", "write_files", "read_file", "stop_sandbox", "logs", "app_logs", "list_sandboxes", "inspect_sandbox", "list_snapshots", "list_profiles", "list_images", "list_volumes", "create_app", "update_app", "list_apps", "get_app", "app_domain_add", "app_domain_rm", "app_domain_ls"}
	hidden := []string{"run", "create_sandbox", "snapshot", "fork", "delete_sandbox", "delete_snapshot", "delete_app", "delete_image", "capture", "volume_create", "delete_volume", "volume_backup", "volume_restore"}
	for _, n := range shown {
		if !got[n] {
			t.Errorf("tool %q should be advertised under a read+exec policy", n)
		}
	}
	for _, n := range hidden {
		if got[n] {
			t.Errorf("tool %q should be hidden under a read+exec policy", n)
		}
	}
	if len(got) != len(shown) {
		t.Errorf("advertised %d tools, want %d: %v", len(got), len(shown), got)
	}
}

func TestToolMirrorFullPolicyAdvertisesAll(t *testing.T) {
	cs := connect(t, Config{Policy: &policy.Policy{Operations: policy.KnownOperations()}})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 35 {
		t.Errorf("all-ops policy advertised %d tools, want 35", len(res.Tools))
	}
}

// --- pure guard-helper unit tests -------------------------------------------

func TestToolEnabled(t *testing.T) {
	cases := []struct {
		name       string
		allow, den []string
		tool       string
		want       bool
	}{
		{"default all", nil, nil, "run", true},
		{"allowlist hit", []string{"run"}, nil, "run", true},
		{"allowlist miss", []string{"run"}, nil, "fork", false},
		{"deny wins over allow", []string{"run"}, []string{"run"}, "run", false},
		{"deny only", nil, []string{"fork"}, "fork", false},
		{"deny only, other tool", nil, []string{"fork"}, "run", true},
	}
	for _, c := range cases {
		got := Config{Tools: c.allow, DenyTools: c.den}.toolEnabled(c.tool)
		if got != c.want {
			t.Errorf("%s: toolEnabled(%q) = %v, want %v", c.name, c.tool, got, c.want)
		}
	}
}

func TestResolveProfile(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		in      string
		want    string
		wantErr bool
	}{
		{"passthrough", Config{}, "x", "x", false},
		{"default applied", Config{DefaultProfile: "d"}, "", "d", false},
		{"allowlist hit", Config{AllowProfiles: []string{"a", "b"}}, "a", "a", false},
		{"allowlist miss", Config{AllowProfiles: []string{"a", "b"}}, "c", "", true},
		{"allowlist, no profile, no default", Config{AllowProfiles: []string{"a"}}, "", "", true},
		{"allowlist satisfied by default", Config{AllowProfiles: []string{"a"}, DefaultProfile: "a"}, "", "a", false},
	}
	for _, c := range cases {
		got, err := c.cfg.resolveProfile(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCheckNetAllow(t *testing.T) {
	none := Config{}
	if err := none.checkNetAllow([]string{"anything.com"}); err != nil {
		t.Errorf("no ceiling should allow any host: %v", err)
	}
	ceil := Config{NetAllowMax: []string{"pypi.org", "npmjs.org"}}
	if err := ceil.checkNetAllow([]string{"pypi.org"}); err != nil {
		t.Errorf("subset should pass: %v", err)
	}
	if err := ceil.checkNetAllow(nil); err != nil {
		t.Errorf("empty request should pass: %v", err)
	}
	if err := ceil.checkNetAllow([]string{"pypi.org", "evil.com"}); err == nil {
		t.Error("out-of-ceiling host should be rejected")
	}
}

func TestClampTimeout(t *testing.T) {
	noClamp := Config{}
	if got := noClamp.clampTimeout(0); got != 0 {
		t.Errorf("no clamp: got %d, want 0", got)
	}
	c := Config{MaxTimeout: 300 * time.Second}
	for _, tc := range []struct{ in, want int }{
		{0, 300},   // unbounded → ceiling
		{100, 100}, // within → unchanged
		{500, 300}, // over → ceiling
	} {
		if got := c.clampTimeout(tc.in); got != tc.want {
			t.Errorf("clampTimeout(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCheckFork(t *testing.T) {
	c := Config{MaxFork: 8}
	if n, err := c.checkFork(0); err != nil || n != 1 {
		t.Errorf("count 0 → (%d, %v), want (1, nil)", n, err)
	}
	if n, err := c.checkFork(5); err != nil || n != 5 {
		t.Errorf("count 5 → (%d, %v), want (5, nil)", n, err)
	}
	if _, err := c.checkFork(9); err == nil {
		t.Error("count 9 over --max-fork 8 should error")
	}
	if n, err := (Config{}).checkFork(100); err != nil || n != 100 {
		t.Errorf("no cap: count 100 → (%d, %v), want (100, nil)", n, err)
	}
}

// --- integration tests through the MCP layer --------------------------------

func TestDefaultProfileReachesDaemon(t *testing.T) {
	var seenProfile string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateSandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenProfile = req.Profile
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "s"})
	})
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, _ *http.Request) {
		fw := wire.NewFrameWriter(w)
		payload, _ := json.Marshal(wire.ExecResult{})
		_ = fw.WriteFrame(wire.FrameExit, payload)
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })

	cs := connect(t, Config{Client: daemonClient(t, mux), DefaultProfile: "python-3.12"})
	call(t, cs, "run", map[string]any{"command": []string{"true"}}, nil)
	if seenProfile != "python-3.12" {
		t.Errorf("daemon saw profile %q, want python-3.12 (default)", seenProfile)
	}
}

func TestMaxTimeoutClampReachesDaemon(t *testing.T) {
	var seenTimeout int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SandboxResponse{ID: "s"})
	})
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req wire.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenTimeout = req.TimeoutSec
		fw := wire.NewFrameWriter(w)
		payload, _ := json.Marshal(wire.ExecResult{})
		_ = fw.WriteFrame(wire.FrameExit, payload)
	})
	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })

	cs := connect(t, Config{Client: daemonClient(t, mux), MaxTimeout: 30 * time.Second})
	// Request no timeout — the clamp must pull it down to the 30s ceiling.
	call(t, cs, "run", map[string]any{"command": []string{"sleep", "999"}}, nil)
	if seenTimeout != 30 {
		t.Errorf("daemon saw exec timeout %d, want 30 (clamped)", seenTimeout)
	}
}

func TestAllowProfilesRejected(t *testing.T) {
	cs := connect(t, Config{Client: daemonClient(t, http.NewServeMux()), AllowProfiles: []string{"base"}})
	msg := callErr(t, cs, "run", map[string]any{"profile": "ghost", "command": []string{"true"}})
	if !strings.Contains(msg, "not allowed") {
		t.Errorf("error = %q, want it to mention the profile is not allowed", msg)
	}
}

func TestNetAllowMaxRejected(t *testing.T) {
	cs := connect(t, Config{Client: daemonClient(t, http.NewServeMux()), NetAllowMax: []string{"pypi.org"}})
	msg := callErr(t, cs, "run", map[string]any{"command": []string{"true"}, "net_allow": []string{"evil.com"}})
	if !strings.Contains(msg, "net-allow-max") {
		t.Errorf("error = %q, want it to mention the --net-allow-max ceiling", msg)
	}
}

func TestMaxForkRejected(t *testing.T) {
	cs := connect(t, Config{Client: daemonClient(t, http.NewServeMux()), MaxFork: 2})
	msg := callErr(t, cs, "fork", map[string]any{"snapshot_id": "snap", "count": 5})
	if !strings.Contains(msg, "max-fork") {
		t.Errorf("error = %q, want it to mention the --max-fork limit", msg)
	}
}

func TestMaxSandboxesRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		// One sandbox already live; the limit is 1, so a create must be refused.
		_ = json.NewEncoder(w).Encode(api.ListResponse{Sandboxes: []api.SandboxResponse{{ID: "existing"}}})
	})
	mux.HandleFunc("POST /sandboxes", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("create should have been refused before hitting the daemon")
	})
	cs := connect(t, Config{Client: daemonClient(t, mux), MaxSandboxes: 1})
	msg := callErr(t, cs, "create_sandbox", map[string]any{"profile": "base"})
	if !strings.Contains(msg, "max-sandboxes") {
		t.Errorf("error = %q, want it to mention the --max-sandboxes limit", msg)
	}
}

func TestToolFiltering(t *testing.T) {
	// Allowlist plus a deny that overlaps: only run and list_profiles survive.
	cs := connect(t, Config{Tools: []string{"run", "list_profiles", "fork"}, DenyTools: []string{"fork"}})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	sort.Strings(got)
	want := []string{"list_profiles", "run"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("exposed tools = %v, want %v", got, want)
	}
}
