//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

func netconfigServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /network/configure", handleNetworkConfigure)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postConfigure(t *testing.T, ts *httptest.Server, req agentwire.NetworkConfigRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/network/configure", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /network/configure: %v", err)
	}
	return resp
}

func TestNetworkConfigureAppliesAndWritesResolver(t *testing.T) {
	// Stub the netlink applier (needs a real iface + root) and redirect
	// the resolver files into a temp dir.
	var applied *agentwire.NetworkConfigRequest
	orig := netconfigApplier
	netconfigApplier = func(req *agentwire.NetworkConfigRequest) error {
		applied = req
		return nil
	}
	t.Cleanup(func() { netconfigApplier = orig })

	dir := t.TempDir()
	origResolv, origHosts, origHostname, origSetter := resolvConfPath, hostsPath, hostnamePath, hostnameSetter
	resolvConfPath = filepath.Join(dir, "resolv.conf")
	hostsPath = filepath.Join(dir, "hosts")
	hostnamePath = filepath.Join(dir, "hostname")
	var setName string
	hostnameSetter = func(b []byte) error { setName = string(b); return nil }
	t.Cleanup(func() {
		resolvConfPath, hostsPath, hostnamePath, hostnameSetter = origResolv, origHosts, origHostname, origSetter
	})

	ts := netconfigServer(t)
	resp := postConfigure(t, ts, agentwire.NetworkConfigRequest{
		Address:   "10.20.0.14",
		PrefixLen: 30,
		Gateway:   "10.20.0.13",
		DNS:       []string{"10.20.255.254"},
		Hostname:  "sbx_test",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure = %d", resp.StatusCode)
	}

	if applied == nil || applied.Interface != "eth0" || applied.Address != "10.20.0.14" {
		t.Fatalf("applier got %+v (interface should default to eth0)", applied)
	}
	if data, _ := os.ReadFile(resolvConfPath); !strings.Contains(string(data), "nameserver 10.20.255.254") {
		t.Errorf("resolv.conf = %q", data)
	}
	if setName != "sbx_test" {
		t.Errorf("hostname set to %q, want sbx_test", setName)
	}
	if data, _ := os.ReadFile(hostsPath); !strings.Contains(string(data), "10.20.0.14\tsbx_test") {
		t.Errorf("hosts = %q", data)
	}
}

func TestNetworkConfigureRejectsBadRequest(t *testing.T) {
	orig := netconfigApplier
	netconfigApplier = func(*agentwire.NetworkConfigRequest) error { return nil }
	t.Cleanup(func() { netconfigApplier = orig })
	ts := netconfigServer(t)

	for _, tc := range []struct {
		name string
		req  agentwire.NetworkConfigRequest
	}{
		{"no address", agentwire.NetworkConfigRequest{PrefixLen: 30}},
		{"zero prefix", agentwire.NetworkConfigRequest{Address: "10.0.0.1"}},
		{"prefix too big", agentwire.NetworkConfigRequest{Address: "10.0.0.1", PrefixLen: 33}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postConfigure(t, ts, tc.req)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", tc.name, resp.StatusCode)
			}
		})
	}
}

func TestNetworkConfigureResolverSymlinkReplaced(t *testing.T) {
	// resolv.conf is often a symlink (systemd-resolved). The writer must
	// replace it with a real file, not write through the link.
	orig := netconfigApplier
	netconfigApplier = func(*agentwire.NetworkConfigRequest) error { return nil }
	t.Cleanup(func() { netconfigApplier = orig })

	dir := t.TempDir()
	target := filepath.Join(dir, "real-target")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "resolv.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	origResolv := resolvConfPath
	resolvConfPath = link
	t.Cleanup(func() { resolvConfPath = origResolv })

	ts := netconfigServer(t)
	resp := postConfigure(t, ts, agentwire.NetworkConfigRequest{Address: "1.2.3.4", PrefixLen: 30, DNS: []string{"9.9.9.9"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure = %d", resp.StatusCode)
	}
	// The symlink target must be untouched; resolv.conf is now a real file.
	if data, _ := os.ReadFile(target); string(data) != "original\n" {
		t.Errorf("wrote through the symlink; target = %q", data)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("resolv.conf still a symlink: %v", fi.Mode())
	}
	if data, _ := os.ReadFile(link); !strings.Contains(string(data), "9.9.9.9") {
		t.Errorf("resolv.conf = %q", data)
	}
}
