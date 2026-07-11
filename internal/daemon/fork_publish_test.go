package daemon

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestForkPublishValidation pins the route-level rules for the fork body:
// publish demands count 1 (host ports are exclusive), and a malformed
// body is a clean 400 — while the legacy body-less query form keeps
// working (404 here, since the snapshot doesn't exist, proving it got
// past body parsing).
func TestForkPublishValidation(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	url := ts.URL + "/snapshots/snap_aaaaaaaaaaaaa/fork"

	post := func(body string) *http.Response {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		resp, err := http.Post(url, "application/json", rdr)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	if resp := post(`{"count":2,"publish":[{"host_port":8081,"guest_port":80}]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("publish with count 2 = %d, want 400", resp.StatusCode)
	}
	if resp := post(`{"publish":[{"host_port":0,"guest_port":80}]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid host port = %d, want 400", resp.StatusCode)
	}
	if resp := post(`not json`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", resp.StatusCode)
	}
	if resp := post(""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("legacy body-less fork = %d, want 404 (unknown snapshot)", resp.StatusCode)
	}
	if resp := post(`{"count":1,"publish":[{"host_port":8081,"guest_port":80}]}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("valid publish body, unknown snapshot = %d, want 404", resp.StatusCode)
	}
}
