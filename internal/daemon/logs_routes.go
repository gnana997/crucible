package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/sandbox"
)

// logDrainInterval paces the poll of each sandbox's service log ring. A
// var (not const) so tests can shorten it.
var logDrainInterval = time.Second

const (
	// logDrainMaxBytes bounds one drain read (the ring is small; this is
	// generous headroom so a burst is drained in one poll).
	logDrainMaxBytes = 256 << 10
	// logDefaultTailBytes is how much of a log a plain (non-follow) read
	// tails when the client gives no cursor.
	logDefaultTailBytes = 64 << 10
	// logMaxRecords caps the records returned by one /logs read.
	logMaxRecords = 5000
)

func nowMs() int64 { return time.Now().UnixMilli() }

// appendLog writes one record to the durable store, best-effort — a log
// failure must never break the request or drain that produced it.
func (s *Server) appendLog(id string, rec logstore.Record) {
	if s.cfg.LogStore == nil {
		return
	}
	if err := s.cfg.LogStore.Append(id, rec); err != nil {
		s.cfg.Logger.Warn("log append failed", "sandbox", id, "err", err)
	}
}

// execLogWriter tees an exec stream into the durable log store. Its Write
// always reports success (logging is best-effort — a log failure must not
// disrupt the exec output stream it shadows).
type execLogWriter struct {
	s      *Server
	id     string
	stream string
}

func (w execLogWriter) Write(p []byte) (int, error) {
	w.s.appendLog(w.id, logstore.Record{
		TimeMs: nowMs(), Source: logstore.SourceExec, Stream: w.stream, Text: string(p),
	})
	return len(p), nil
}

// startServiceDrain launches the background drain of a sandbox's service
// output into the durable store. Only call it when LogStore is set. The
// goroutine is tracked by bgWG and exits when the sandbox is gone or the
// server shuts down.
func (s *Server) startServiceDrain(id string) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.drainServiceLogs(s.bgCtx, id, func(ctx context.Context, fromSeq uint64) (agentwire.ServiceLogsResponse, error) {
			return s.cfg.Manager.ServiceLogs(ctx, id, fromSeq, logDrainMaxBytes)
		})
	}()
}

// drainServiceLogs polls the guest service log ring and appends new
// records to the durable store until the sandbox disappears (ErrNotFound,
// which covers every delete path) or ctx is cancelled (server shutdown).
// The poll func is injected so this is unit-testable without a live agent.
func (s *Server) drainServiceLogs(ctx context.Context, id string, poll func(context.Context, uint64) (agentwire.ServiceLogsResponse, error)) {
	ticker := time.NewTicker(logDrainInterval)
	defer ticker.Stop()
	var cursor uint64
	for {
		resp, err := poll(ctx, cursor)
		switch {
		case err == nil:
			if resp.FirstSeq > cursor {
				// The ring evicted records we hadn't drained: record the
				// gap so the durable log is honest about the hole. cursor
				// is advanced to NextSeq below regardless.
				s.appendLog(id, logstore.Record{
					TimeMs: nowMs(), Source: logstore.SourceService, Stream: logstore.StreamEvent,
					Text: fmt.Sprintf("[%d records dropped: log ring eviction]", resp.FirstSeq-cursor),
				})
			}
			for _, rec := range resp.Records {
				s.appendLog(id, logstore.Record{
					TimeMs: rec.UnixMs, Source: logstore.SourceService, Stream: rec.Stream, Text: string(rec.Data),
				})
			}
			cursor = resp.NextSeq
		case errors.Is(err, sandbox.ErrNotFound), ctx.Err() != nil:
			return // sandbox gone, or server shutting down
		default:
			// Transient (agent briefly unreachable): pace and retry below.
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handleSandboxLogs serves a sandbox's durable logs. It deliberately does
// NOT require the sandbox to still exist — logs outlive the sandbox so a
// crashed or deleted workload can be inspected post-mortem.
//
// The cursor is a byte offset: no `since` tails the recent log; `since=N`
// returns records after offset N (for a client-side follow poll). An
// optional `source=service|exec|all` filters the returned records.
func (s *Server) handleSandboxLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	if s.cfg.LogStore == nil {
		writeError(w, http.StatusNotImplemented,
			errors.New("durable logs are not enabled on this daemon (set --log-dir)"))
		return
	}
	q := r.URL.Query()
	since := int64(-1) // default: tail the recent log
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, errors.New("invalid since (want a non-negative byte offset)"))
			return
		}
		since = n
	}
	source := q.Get("source")
	switch source {
	case "", "all", logstore.SourceService, logstore.SourceExec:
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid source (want service|exec|all)"))
		return
	}

	recs, next, err := s.cfg.LogStore.Read(id, since, logDefaultTailBytes, logMaxRecords)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.LogRecord, 0, len(recs))
	for _, rc := range recs {
		if source != "" && source != "all" && rc.Source != source {
			continue
		}
		out = append(out, api.LogRecord{TimeMs: rc.TimeMs, Source: rc.Source, Stream: rc.Stream, Text: rc.Text})
	}
	writeJSON(w, http.StatusOK, api.LogsResponse{Records: out, NextOffset: next})
}
