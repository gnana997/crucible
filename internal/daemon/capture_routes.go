package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/capture"
)

// handleCapture streams a live packet capture of a sandbox's traffic as a pcap
// stream (Content-Type application/vnd.tcpdump.pcap). Host-side, on the sandbox's
// veth — no in-guest binary. Gated by the default-deny `capture` scoped-token op
// (payloads are sensitive) and audited. Bounded by hard byte + duration caps.
//
//	GET /sandboxes/{id}/capture?filter=<bpf>&snaplen=&max_bytes=&max_seconds=
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.cfg.Manager.Get(id); err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("sandbox %s not found", id))
		return
	}
	iface, ok := s.cfg.Manager.HostIface(id)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("sandbox has no capturable network (no network, or asleep)"))
		return
	}
	if !capture.Available() {
		writeError(w, http.StatusNotImplemented, capture.ErrNoTcpdump)
		return
	}

	q := r.URL.Query()
	filter := q.Get("filter")
	if !capture.ValidFilter(filter) {
		writeError(w, http.StatusBadRequest, errors.New("invalid capture filter"))
		return
	}
	opt := capture.Options{
		Iface:    iface,
		Filter:   filter,
		Snaplen:  atoiOr(q.Get("snaplen"), 0),
		MaxBytes: int64(atoiOr(q.Get("max_bytes"), 0)),
	}
	if secs := atoiOr(q.Get("max_seconds"), 0); secs > 0 {
		opt.MaxDur = time.Duration(secs) * time.Second
	}
	eff := opt.Normalized()

	// Audit (H5 seed): a capture exposes traffic payloads — record who ran it.
	s.cfg.Logger.Info("packet capture started",
		"sandbox", id, "iface", iface, "filter", filter,
		"max_bytes", eff.MaxBytes, "max_seconds", int64(eff.MaxDur.Seconds()),
		"token", tokenIDFor(r))

	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	fw := &flushWriter{w: w}
	if err := capture.Run(r.Context(), opt, fw); err != nil {
		// Headers/bytes may already be streaming; log rather than write a status.
		s.cfg.Logger.Warn("packet capture error", "sandbox", id, "err", err)
	}
	s.cfg.Logger.Info("packet capture ended", "sandbox", id, "bytes", fw.n)
}

// flushWriter forwards to the ResponseWriter, flushing after each write so pcap
// frames reach the client promptly, and counts bytes for the audit line.
type flushWriter struct {
	w http.ResponseWriter
	n int64
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.n += int64(n)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
