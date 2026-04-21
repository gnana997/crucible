package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/daemon"
	"github.com/gnana997/crucible/internal/jailer"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
)

// runDaemon implements the `crucible daemon` subcommand.
//
// It wires the four layers we built in wk1 — runner → sandbox.Manager →
// daemon.Server → cmd — and blocks until SIGINT/SIGTERM or a fatal
// error from the HTTP server. On shutdown it:
//
//  1. Stops accepting new HTTP requests (http.Server.Shutdown).
//  2. Waits for in-flight requests up to the drain deadline.
//  3. Drains every still-live sandbox (Manager.Shutdown) so we don't
//     leave orphan firecracker processes running.
//
// The return value is the exit code for the parent main().
func runDaemon(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crucible daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr         = fs.String("listen", "127.0.0.1:7878", "HTTP listen address")
		fcBin        = fs.String("firecracker-bin", "", "path to the firecracker binary (required)")
		kernel       = fs.String("kernel", "", "path to the guest kernel image — uncompressed vmlinux (required)")
		rootfs       = fs.String("rootfs", "", "path to the guest root filesystem image (required)")
		workBase     = fs.String("work-base", "/tmp/crucible/run", "directory where per-sandbox workdirs are created")
		logFormat    = fs.String("log-format", "text", "log format: text|json")
		logLevel     = fs.String("log-level", "info", "log level: debug|info|warn|error")
		drainStr     = fs.String("drain-timeout", "30s", "max wallclock to wait for in-flight requests + sandbox drain on shutdown")
		noWaitAgent  = fs.Bool("no-wait-for-agent", false, "skip guest agent readiness polling on create (dev-only; needed when rootfs has no crucible-agent)")
		agentTimeout = fs.String("agent-ready-timeout", "15s", "max wait for guest agent /healthz on create (ignored when --no-wait-for-agent)")
		// Jailer flags: when --jailer-bin is set, the daemon wraps every
		// firecracker instance in its own jailer chroot + mount/pid
		// namespace + cgroup v2 slice, and drops to --jail-uid/--jail-gid
		// before exec. Requires the daemon to run as root.
		jailerBin  = fs.String("jailer-bin", "", "path to jailer binary; when set, run firecracker under jailer (requires root)")
		chrootBase = fs.String("chroot-base", "/srv/jailer", "parent dir for per-VM jailer chroots (used only when --jailer-bin is set)")
		jailUID    = fs.Uint("jail-uid", 10000, "unprivileged uid jailer drops to before exec'ing firecracker")
		jailGID    = fs.Uint("jail-gid", 10000, "unprivileged gid jailer drops to before exec'ing firecracker")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: crucible daemon [flags]

Run the crucible HTTP daemon.

Required flags:
  --firecracker-bin PATH   path to the firecracker binary
  --kernel PATH            guest kernel image (uncompressed vmlinux)
  --rootfs PATH            guest root filesystem image

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag already printed the error; -h prints help and returns ErrHelp.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// --- validate required paths ------------------------------------------
	for _, req := range []struct {
		name, val string
	}{
		{"firecracker-bin", *fcBin},
		{"kernel", *kernel},
		{"rootfs", *rootfs},
	} {
		if req.val == "" {
			fmt.Fprintf(stderr, "error: --%s is required\n\n", req.name)
			fs.Usage()
			return 2
		}
		if _, err := os.Stat(req.val); err != nil {
			fmt.Fprintf(stderr, "error: --%s %q: %v\n", req.name, req.val, err)
			return 2
		}
	}

	// Eagerly create the work base so permission errors surface now, not
	// on the first create.
	if err := os.MkdirAll(*workBase, 0o750); err != nil {
		fmt.Fprintf(stderr, "error: create --work-base %q: %v\n", *workBase, err)
		return 2
	}

	// --- logger -----------------------------------------------------------
	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(stderr, "error: --log-level: %v\n", err)
		return 2
	}
	logger, err := buildLogger(*logFormat, level, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: --log-format: %v\n", err)
		return 2
	}
	slog.SetDefault(logger)

	drainTimeout, err := time.ParseDuration(*drainStr)
	if err != nil {
		fmt.Fprintf(stderr, "error: --drain-timeout: %v\n", err)
		return 2
	}
	agentReady, err := time.ParseDuration(*agentTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "error: --agent-ready-timeout: %v\n", err)
		return 2
	}

	logger.Info("crucible starting",
		"addr", *addr,
		"firecracker_bin", *fcBin,
		"kernel", *kernel,
		"rootfs", *rootfs,
		"work_base", *workBase,
	)

	// --- wiring -----------------------------------------------------------
	// Pick a runner. --jailer-bin is the switch: unset = dev-friendly
	// direct exec (no sudo required, no chroot), set = production mode
	// with jailer isolation + cgroup v2 quotas + privilege drop. Both
	// paths implement runner.Runner, so the manager is oblivious.
	var r runner.Runner
	if *jailerBin != "" {
		if _, err := os.Stat(*jailerBin); err != nil {
			fmt.Fprintf(stderr, "error: --jailer-bin %q: %v\n", *jailerBin, err)
			return 2
		}
		// Reap any chroots left behind by a previous daemon run that
		// crashed or was killed without clean shutdown. Sandboxes are
		// in-memory only, so every dir under <chroot-base>/firecracker/
		// at startup is by definition an orphan.
		if reaped, err := jailer.ReapOrphans(*chrootBase, *fcBin); err != nil {
			logger.Warn("orphan reap failed (continuing)", "err", err)
		} else if len(reaped) > 0 {
			logger.Info("reaped orphan chroots from previous run", "count", len(reaped), "ids", reaped)
		}
		jr := runner.NewJailerRunner(*jailerBin, *fcBin, *chrootBase, uint32(*jailUID), uint32(*jailGID))
		jr.Logger = logger
		r = jr
		logger.Info("runner mode: jailer",
			"jailer_bin", *jailerBin,
			"chroot_base", *chrootBase,
			"uid", *jailUID,
			"gid", *jailGID,
		)
	} else {
		fc := runner.New(*fcBin)
		fc.Logger = logger
		r = fc
		logger.Info("runner mode: direct firecracker (no jailer)")
	}

	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:            r,
		WorkBase:          *workBase,
		Kernel:            *kernel,
		Rootfs:            *rootfs,
		WaitForAgent:      !*noWaitAgent,
		AgentReadyTimeout: agentReady,
	})
	if err != nil {
		logger.Error("manager init failed", "err", err)
		return 1
	}

	srv, err := daemon.New(daemon.Config{
		Manager: mgr,
		Addr:    *addr,
		Logger:  logger,
	})
	if err != nil {
		logger.Error("daemon init failed", "err", err)
		return 1
	}

	// --- run + shutdown ---------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("signal received, starting shutdown")
	case err := <-errCh:
		if err != nil {
			logger.Error("server failed", "err", err)
			return 1
		}
		// Server returned ErrServerClosed without us triggering it;
		// treat as clean exit.
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		logger.Warn("http shutdown did not complete cleanly", "err", err)
	}
	mgr.Shutdown(drainCtx)
	logger.Info("crucible stopped")
	_ = stdout // reserved for future non-log output
	return 0
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
	}
}

func buildLogger(format string, level slog.Level, w io.Writer) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want text|json)", format)
	}
}
