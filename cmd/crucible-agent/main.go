//go:build linux

// Command crucible-agent runs inside every crucible sandbox VM.
//
// It listens on AF_VSOCK port 52 (see agentwire.AgentVSockPort) for HTTP
// requests from the host daemon — typically POST /exec — and runs them
// inside the guest, streaming stdout/stderr back as framed responses.
//
// Why this exists: the host-side crucible daemon needs a way to run
// arbitrary commands inside a booted microVM and stream the results back
// without granting the VM any network access. vsock (virtio-vsock) is
// the clean primitive for that — a socket family that carries bytes
// between host and guest via a shared-memory ring buffer, no network
// stack involved. The agent is the guest-side endpoint.
//
// This binary is meant to be baked into a crucible rootfs image and
// started by systemd (or the kernel's init=) on boot. It has no
// configuration beyond the well-known port.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ln, err := listenVSock(agentwire.AgentVSockPort)
	if err != nil {
		logger.Error("vsock listen failed", "err", err)
		os.Exit(1)
	}
	logger.Info("crucible-agent listening", "vsock_port", agentwire.AgentVSockPort)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /network/refresh", handleNetworkRefresh)

	// No Read/Write timeouts on the server: exec responses can stream for
	// as long as the command takes. Per-request deadlines come from
	// ExecRequest.TimeoutSec and are enforced inside handleExec.
	srv := &http.Server{Handler: mux}

	// Graceful shutdown on SIGTERM/SIGINT. systemd will SIGTERM on a
	// `systemctl stop crucible-agent`.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("signal received, shutting down")
	case err := <-errCh:
		if err != nil {
			logger.Error("serve failed", "err", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not complete cleanly", "err", err)
	}
	logger.Info("crucible-agent stopped")
}

// handleHealthz is a trivial readiness probe. The host can poll this
// over vsock after a sandbox is created to confirm the agent is up
// before attempting /exec.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
}
