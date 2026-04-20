package daemon

import (
	"net/http"
	"time"
)

// logRequests wraps h with a per-request log line at Info level.
//
// Fields: method, path, status, duration_ms, remote_addr. We intentionally
// keep this to structured logging only (no println) so the daemon can run
// with --log-format=json in production without its middleware emitting
// stray plain-text output.
func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(lrw, r)
		s.cfg.Logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// loggingResponseWriter captures the status code a handler writes so the
// middleware can log it. Without this we'd only know status 200 (the
// default for Write without prior WriteHeader).
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
