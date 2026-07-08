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
	os.Exit(run(logger))
}

// run holds main's body so deferred cleanup runs before os.Exit.
func run(logger *slog.Logger) int {
	ln, err := listenVSock(agentwire.AgentVSockPort)
	if err != nil {
		logger.Error("vsock listen failed", "err", err)
		return 1
	}
	logger.Info("crucible-agent listening", "vsock_port", agentwire.AgentVSockPort)

	// A previous agent instance (crashed, relaunched by systemd) may have
	// orphaned a supervised service; kill it before accepting commands so
	// two entrypoints never run at once.
	reconcileStaleService(servicePidFile, logger)
	sup := newSupervisor(execRunner{}, realClock{}, logger, servicePidFile)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /network/refresh", handleNetworkRefresh)
	mux.HandleFunc("POST /identity/refresh", handleIdentityRefresh)
	(&serviceAPI{sup: sup}).register(mux)

	// No Read/Write timeouts on the server: exec responses can stream for
	// as long as the command takes. Per-request deadlines come from
	// ExecRequest.TimeoutSec and are enforced inside handleExec.
	// ReadHeaderTimeout is safe to set — it bounds only the request-header
	// read, not the body or the streamed response — and closes the slowloris
	// hole (gosec G112) of a peer that opens a connection and dribbles headers.
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

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
			return 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not complete cleanly", "err", err)
	}
	// Stop the supervised service gracefully (StopSignal + grace, then
	// SIGKILL) before the agent goes away. Blocks until the process is
	// gone, bounded by the spec's grace + the wait backstop.
	if _, err := sup.Shutdown(); err != nil {
		logger.Warn("service shutdown failed", "err", err)
	}
	logger.Info("crucible-agent stopped")
	return 0
}

// handleHealthz is a trivial readiness probe. The host can poll this
// over vsock after a sandbox is created to confirm the agent is up
// before attempting /exec.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
}
