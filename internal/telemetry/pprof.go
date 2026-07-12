package telemetry

import (
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
)

// StartPprof serves Go's net/http/pprof on addr in a background goroutine and
// returns the server so the caller can Shutdown it. Returns nil when addr is
// empty (pprof off). onError is called if the listener fails after start.
//
// pprof profiles can expose process memory, so this is off by default and the
// daemon flag help steers operators to a loopback bind; IsLoopbackAddr lets the
// caller warn on a non-loopback bind.
func StartPprof(addr string, onError func(error)) *http.Server {
	if addr == "" {
		return nil
	}
	srv := &http.Server{Addr: addr, Handler: pprofMux()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed && onError != nil {
			onError(err)
		}
	}()
	return srv
}

// pprofMux builds the net/http/pprof handler set. pprof.Index also serves the
// named profiles under /debug/pprof/<name> (heap, goroutine, allocs, …); the
// others are their own handlers.
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// IsLoopbackAddr reports whether a listen address binds only the loopback
// interface. An empty host (e.g. ":6060") binds all interfaces → not loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // no host = all interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
