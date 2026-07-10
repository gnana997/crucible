//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/gnana997/crucible/sdk/wire"
)

func newServiceTestServer(t *testing.T) (*httptest.Server, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{}
	sup := newTestSupervisor(t, fr, newFakeClock())
	mux := http.NewServeMux()
	(&serviceAPI{sup: sup}).register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, fr
}

func doJSON(t *testing.T, method, url string, body []byte) (*http.Response, wire.ServiceStatus) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	var status wire.ServiceStatus
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
	}
	return resp, status
}

func TestServiceHTTPLifecycle(t *testing.T) {
	ts, fr := newServiceTestServer(t)
	fc := newFakeChild(101)
	fc.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc)

	spec, _ := json.Marshal(wire.ServiceSpec{Cmd: []string{"/bin/app"}})
	resp, status := doJSON(t, http.MethodPut, ts.URL+"/service", spec)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /service = %d, want 200", resp.StatusCode)
	}
	if status.State != wire.ServiceStateStopped {
		t.Fatalf("state after configure = %q", status.State)
	}

	resp, status = doJSON(t, http.MethodPost, ts.URL+"/service/start", nil)
	if resp.StatusCode != http.StatusOK || status.State != wire.ServiceStateRunning {
		t.Fatalf("start = %d state %q, want 200 running", resp.StatusCode, status.State)
	}

	resp, status = doJSON(t, http.MethodGet, ts.URL+"/service/status", nil)
	if resp.StatusCode != http.StatusOK || status.Pid != 101 {
		t.Fatalf("status = %d pid %d, want 200 pid 101", resp.StatusCode, status.Pid)
	}

	resp, status = doJSON(t, http.MethodPost, ts.URL+"/service/stop", []byte(`{"grace_s": 5}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d, want 200", resp.StatusCode)
	}
	if status.State != wire.ServiceStateStopping && status.State != wire.ServiceStateStopped {
		t.Fatalf("state after stop = %q", status.State)
	}
}

func TestServiceHTTPStartWithoutSpecIs409(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/service/start", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("start without spec = %d, want 409", resp.StatusCode)
	}
}

func TestServiceHTTPBadSpecIs400(t *testing.T) {
	ts, _ := newServiceTestServer(t)

	resp, _ := doJSON(t, http.MethodPut, ts.URL+"/service", []byte(`{not json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPut, ts.URL+"/service", []byte(`{"cmd": []}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty cmd = %d, want 400", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPut, ts.URL+"/service", []byte(`{"cmd": ["/x"], "stop_signal": "SIGNOPE"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown signal = %d, want 400", resp.StatusCode)
	}
}

func TestServiceHTTPLogs(t *testing.T) {
	ts, fr := newServiceTestServer(t)
	fr.enqueue(newFakeChild(101))

	// Logs before configure: 409.
	resp, _ := doJSON(t, http.MethodGet, ts.URL+"/service/logs", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("logs before configure = %d, want 409", resp.StatusCode)
	}

	spec, _ := json.Marshal(wire.ServiceSpec{Cmd: []string{"/bin/app"}})
	doJSON(t, http.MethodPut, ts.URL+"/service", spec)
	doJSON(t, http.MethodPost, ts.URL+"/service/start", nil)
	stdout, _ := fr.writers()
	_, _ = stdout.Write([]byte("hello\n"))

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/service/logs?from_seq=0&max_bytes=1024", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var logs wire.ServiceLogsResponse
	if err := json.NewDecoder(res.Body).Decode(&logs); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(logs.Records) != 1 || string(logs.Records[0].Data) != "hello\n" {
		t.Fatalf("logs = %+v", logs.Records)
	}
	if logs.NextSeq != 1 {
		t.Errorf("NextSeq = %d, want 1", logs.NextSeq)
	}

	// Bad query params are 400.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/service/logs?from_seq=nope", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad from_seq = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/service/logs?max_bytes=-5", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad max_bytes = %d, want 400", resp.StatusCode)
	}
}

func TestServiceHTTPStopEmptyBodyOK(t *testing.T) {
	ts, fr := newServiceTestServer(t)
	fc := newFakeChild(101)
	fc.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc)

	spec, _ := json.Marshal(wire.ServiceSpec{Cmd: []string{"/bin/app"}})
	doJSON(t, http.MethodPut, ts.URL+"/service", spec)
	doJSON(t, http.MethodPost, ts.URL+"/service/start", nil)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/service/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop with empty body = %d, want 200", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/service/stop", []byte(`{"grace_s": -1}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative grace = %d, want 400", resp.StatusCode)
	}
}
