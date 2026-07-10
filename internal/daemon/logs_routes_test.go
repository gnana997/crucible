package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/wire"
)

// A literal id of valid sandbox shape (sbx_ + base32hex), so the /logs
// route's IsValidID guard passes without creating a real sandbox.
const logTestSbxID = "sbx_0000000000000"

func logsTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newLogServer(t *testing.T, ls *logstore.Store) *httptest.Server {
	t.Helper()
	srv, err := New(Config{
		Manager:  newBareManager(t),
		Addr:     "127.0.0.1:0",
		Logger:   logsTestLogger(),
		LogStore: ls,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getLogs(t *testing.T, url string) api.LogsResponse {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	var out api.LogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestHandleSandboxLogsSourceFilter(t *testing.T) {
	ls, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = ls.Append(logTestSbxID, logstore.Record{TimeMs: 1, Source: logstore.SourceService, Stream: "stdout", Text: "app-line\n"})
	_ = ls.Append(logTestSbxID, logstore.Record{TimeMs: 2, Source: logstore.SourceExec, Stream: "stdout", Text: "exec-line\n"})
	ts := newLogServer(t, ls)
	base := ts.URL + "/sandboxes/" + logTestSbxID + "/logs"

	if all := getLogs(t, base); len(all.Records) != 2 {
		t.Fatalf("all: got %d records, want 2", len(all.Records))
	}
	svc := getLogs(t, base+"?source=service")
	if len(svc.Records) != 1 || svc.Records[0].Text != "app-line\n" {
		t.Fatalf("source=service: %+v", svc.Records)
	}
	ex := getLogs(t, base+"?source=exec")
	if len(ex.Records) != 1 || ex.Records[0].Text != "exec-line\n" {
		t.Fatalf("source=exec: %+v", ex.Records)
	}
}

func TestHandleSandboxLogsDisabled(t *testing.T) {
	srv, err := New(Config{Manager: newBareManager(t), Addr: "127.0.0.1:0", Logger: logsTestLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/sandboxes/" + logTestSbxID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("logs with no LogStore = %d, want 501", resp.StatusCode)
	}
}

func TestExecLogWriterAppends(t *testing.T) {
	ls, _ := logstore.New(t.TempDir())
	s := &Server{cfg: Config{LogStore: ls, Logger: logsTestLogger()}}
	w := execLogWriter{s: s, id: logTestSbxID, stream: logstore.StreamStdout}
	const msg = "hello from exec\n"
	if n, err := w.Write([]byte(msg)); err != nil || n != len(msg) {
		t.Fatalf("write = %d, %v; want %d, nil", n, err, len(msg))
	}
	recs, _, _ := ls.Read(logTestSbxID, -1, 1<<20, 0)
	if len(recs) != 1 || recs[0].Source != logstore.SourceExec || recs[0].Text != msg {
		t.Fatalf("exec records = %+v", recs)
	}
}

func TestAppendLogNilStoreIsNoop(t *testing.T) {
	s := &Server{cfg: Config{Logger: logsTestLogger()}}   // no LogStore
	s.appendLog(logTestSbxID, logstore.Record{Text: "x"}) // must not panic
}

// runDrain runs a drain to completion with a fast tick, failing if it
// doesn't terminate.
func runDrain(t *testing.T, s *Server, poll func(context.Context, uint64) (wire.ServiceLogsResponse, error)) {
	t.Helper()
	old := logDrainInterval
	logDrainInterval = time.Millisecond
	t.Cleanup(func() { logDrainInterval = old })
	done := make(chan struct{})
	go func() { s.drainServiceLogs(context.Background(), logTestSbxID, poll); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not terminate")
	}
}

func TestDrainServiceLogsPersistsAndStops(t *testing.T) {
	ls, _ := logstore.New(t.TempDir())
	s := &Server{cfg: Config{LogStore: ls, Logger: logsTestLogger()}}
	calls := 0
	runDrain(t, s, func(context.Context, uint64) (wire.ServiceLogsResponse, error) {
		calls++
		if calls == 1 {
			return wire.ServiceLogsResponse{
				Records: []wire.ServiceLogRecord{
					{Seq: 0, Stream: "stdout", UnixMs: 1, Data: []byte("hello ")},
					{Seq: 1, Stream: "stdout", UnixMs: 2, Data: []byte("world\n")},
				},
				NextSeq: 2, FirstSeq: 0,
			}, nil
		}
		return wire.ServiceLogsResponse{}, sandbox.ErrNotFound // sandbox gone → stop
	})
	recs, _, _ := ls.Read(logTestSbxID, -1, 1<<20, 0)
	var got string
	for _, r := range recs {
		if r.Source == logstore.SourceService && r.Stream != "event" {
			got += r.Text
		}
	}
	if got != "hello world\n" {
		t.Fatalf("drained service text = %q, want %q", got, "hello world\n")
	}
}

func TestDrainServiceLogsRecordsEvictionGap(t *testing.T) {
	ls, _ := logstore.New(t.TempDir())
	s := &Server{cfg: Config{LogStore: ls, Logger: logsTestLogger()}}
	calls := 0
	runDrain(t, s, func(context.Context, uint64) (wire.ServiceLogsResponse, error) {
		calls++
		if calls == 1 {
			// cursor starts at 0 but the oldest surviving is seq 5 → a gap.
			return wire.ServiceLogsResponse{
				Records: []wire.ServiceLogRecord{{Seq: 5, Stream: "stdout", UnixMs: 1, Data: []byte("late\n")}},
				NextSeq: 6, FirstSeq: 5,
			}, nil
		}
		return wire.ServiceLogsResponse{}, sandbox.ErrNotFound
	})
	recs, _, _ := ls.Read(logTestSbxID, -1, 1<<20, 0)
	sawGap := false
	for _, r := range recs {
		if r.Stream == "event" && strings.Contains(r.Text, "dropped") {
			sawGap = true
		}
	}
	if !sawGap {
		t.Fatalf("no eviction-gap event recorded; records=%+v", recs)
	}
}
