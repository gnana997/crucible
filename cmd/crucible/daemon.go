package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/daemon"
	"github.com/gnana997/crucible/internal/jailer"
	"github.com/gnana997/crucible/internal/metrics"
	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/tokenstore"
)

// defaultTokenFile is where the daemon's API-key store lives by default.
const defaultTokenFile = "/var/lib/crucible/tokens.json"

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
	// `crucible daemon token …` manages the API-key store (not the daemon).
	if len(args) > 0 && args[0] == "token" {
		return runDaemonToken(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("crucible daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr         = fs.String("listen", "127.0.0.1:7878", "HTTP listen address")
		fcBin        = fs.String("firecracker-bin", "", "path to the firecracker binary (required)")
		kernel       = fs.String("kernel", "", "path to the guest kernel image — uncompressed vmlinux (required)")
		rootfs       = fs.String("rootfs", "", "path to the guest root filesystem image (required; the default when a create request names no profile)")
		rootfsDir    = fs.String("rootfs-dir", "", "directory of pre-baked <profile>.ext4 images; a create request's `profile` field selects one by basename (e.g. python-3.12.ext4 → profile \"python-3.12\")")
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
		// cgroupQuotas sizes host-side cgroup v2 limits (cpu.max/memory.max/
		// pids.max) for each sandbox's VMM from its vCPU/memory request.
		// Only takes effect under jailer mode; the direct-exec runner has
		// no cgroup to write. "off" disables the limits.
		cgroupQuotas = fs.String("cgroup-quotas", "derive", "host cgroup v2 limits per sandbox VMM (jailer mode): derive|off")
		// Network flags: when --network-egress-iface is set AND
		// --jailer-bin is set, the daemon can provision per-sandbox
		// netns + nft + DHCP + DNS proxy. Without both, sandbox
		// requests with network={enabled:true} are rejected at Create.
		netEgressIface = fs.String("network-egress-iface", "", "host interface to masquerade outbound sandbox traffic on (e.g. eth0); enables network feature when set")
		netSubnetPool  = fs.String("network-subnet-pool", "10.20.0.0/16", "base CIDR for per-sandbox /30 allocations")
		dnsUpstream    = fs.String("dns-upstream", "system", `upstream DNS resolver for sandboxes. "system" reads first nameserver from /etc/resolv.conf (falls back to 1.1.1.1); otherwise specify "ip" or "ip:port"`)
		// Auth / TLS. When the token store holds any keys, requests require
		// Authorization: Bearer. Binding a non-loopback --listen requires
		// both keys and TLS (validated below). Manage keys with
		// `crucible daemon token add|list|revoke`.
		tokenFile = fs.String("token-file", defaultTokenFile, "API-key store; when it holds keys, requests require Authorization: Bearer")
		tlsCert   = fs.String("tls-cert", "", "TLS certificate (PEM); required to bind a non-loopback --listen")
		tlsKey    = fs.String("tls-key", "", "TLS private key (PEM); required with --tls-cert")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, `Usage: crucible daemon [flags]

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
			_, _ = fmt.Fprintf(stderr, "error: --%s is required\n\n", req.name)
			fs.Usage()
			return 2
		}
		if _, err := os.Stat(req.val); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: --%s %q: %v\n", req.name, req.val, err)
			return 2
		}
	}

	// --- auth / TLS -------------------------------------------------------
	tokens := tokenstore.Open(*tokenFile)
	if (*tlsCert == "") != (*tlsKey == "") {
		_, _ = fmt.Fprintln(stderr, "error: --tls-cert and --tls-key must be set together")
		return 2
	}
	if !isLoopbackAddr(*addr) {
		if !tokens.Enabled() {
			_, _ = fmt.Fprintf(stderr, "error: refusing to bind non-loopback %q without API keys — run 'crucible daemon token add' first\n", *addr)
			return 2
		}
		if *tlsCert == "" {
			_, _ = fmt.Fprintf(stderr, "error: refusing to serve non-loopback %q without TLS — set --tls-cert and --tls-key\n", *addr)
			return 2
		}
	}

	// Eagerly create the work base so permission errors surface now, not
	// on the first create.
	if err := os.MkdirAll(*workBase, 0o750); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: create --work-base %q: %v\n", *workBase, err)
		return 2
	}

	// --- logger -----------------------------------------------------------
	level, err := parseLogLevel(*logLevel)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: --log-level: %v\n", err)
		return 2
	}
	logger, err := buildLogger(*logFormat, level, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: --log-format: %v\n", err)
		return 2
	}
	slog.SetDefault(logger)

	drainTimeout, err := time.ParseDuration(*drainStr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: --drain-timeout: %v\n", err)
		return 2
	}
	agentReady, err := time.ParseDuration(*agentTimeout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: --agent-ready-timeout: %v\n", err)
		return 2
	}

	var quotaPolicy sandbox.QuotaPolicy
	switch *cgroupQuotas {
	case "derive":
		quotaPolicy = sandbox.QuotaPolicyDerive
	case "off":
		quotaPolicy = sandbox.QuotaPolicyOff
	default:
		_, _ = fmt.Fprintf(stderr, "error: --cgroup-quotas: unknown value %q (want derive|off)\n", *cgroupQuotas)
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
			_, _ = fmt.Fprintf(stderr, "error: --jailer-bin %q: %v\n", *jailerBin, err)
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

	// Network is opt-in at daemon startup: we start it only when
	// the operator has configured the egress interface AND we're
	// running under jailer (per-netns setup requires netns +
	// capabilities that direct-exec doesn't have). Sandboxes can
	// still be created without network — that's the default-deny
	// story. Attempting `network={enabled:true}` in a request when
	// this block didn't run results in a clean 400 from the
	// Manager, not a silent fallback.
	// Reap orphan sandbox network state from a previous run (netns,
	// veths, nft table, iptables ACCEPTs). Always safe to call —
	// touches only objects carrying our crucible- prefix / comment
	// tag — and we run it unconditionally so state from a previous
	// networked run is cleaned up even if the operator started this
	// run without --network-egress-iface.
	network.ReapOrphans(context.Background(), logger)

	var netMgr *network.Manager
	if *netEgressIface != "" && *jailerBin != "" {
		subnetPool, perr := netip.ParsePrefix(*netSubnetPool)
		if perr != nil {
			_, _ = fmt.Fprintf(stderr, "error: --network-subnet-pool: %v\n", perr)
			return 2
		}
		nmgr, nerr := network.Start(context.Background(), network.ManagerConfig{
			SubnetPool:  subnetPool,
			DNSAnycast:  network.DefaultDNSAnycast,
			EgressIface: *netEgressIface,
			DNSUpstream: *dnsUpstream,
			Logger:      logger,
		})
		if nerr != nil {
			logger.Error("network init failed", "err", nerr)
			return 1
		}
		netMgr = nmgr
		logger.Info("network enabled",
			"egress_iface", *netEgressIface,
			"subnet_pool", *netSubnetPool,
			"dns_upstream", *dnsUpstream,
		)
	} else if *netEgressIface != "" && *jailerBin == "" {
		// Half-configured — operator asked for network but not
		// jailer. That's a structural mismatch, not a usage
		// error we can work around; reject loudly at startup.
		_, _ = fmt.Fprintln(stderr, "error: --network-egress-iface requires --jailer-bin (network needs per-sandbox netns)")
		return 2
	}

	var profiles map[string]string
	if *rootfsDir != "" {
		profiles, err = discoverProfiles(*rootfsDir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: --rootfs-dir: %v\n", err)
			return 2
		}
		logger.Info("rootfs profiles discovered", "dir", *rootfsDir, "count", len(profiles))
	}

	mx := metrics.New()

	mgrCfg := sandbox.ManagerConfig{
		Runner:            r,
		WorkBase:          *workBase,
		Kernel:            *kernel,
		Rootfs:            *rootfs,
		Profiles:          profiles,
		WaitForAgent:      !*noWaitAgent,
		AgentReadyTimeout: agentReady,
		Metrics:           mx,
		// Durable local authority (gap 3): journal registry changes to a
		// file under the work base so a restart can reconcile. Rebuild
		// snapshot allowlists from persisted patterns via network.New.
		StatePath: filepath.Join(*workBase, "registry.jsonl"),
		ReloadAllowlist: func(patterns []string) (sandbox.NetworkAllowlist, error) {
			return network.New(patterns)
		},
		QuotaPolicy: quotaPolicy,
	}
	if netMgr != nil {
		mgrCfg.Network = daemon.NewNetworkAdapter(netMgr)
	}
	mgr, err := sandbox.NewManager(mgrCfg)
	if err != nil {
		logger.Error("manager init failed", "err", err)
		return 1
	}

	// Reconcile against the previous run's journal: re-adopt snapshots
	// whose files survived and reap orphaned sandbox workdirs. Runs after
	// the jailer + network orphan-reaps above, which already killed any
	// leftover VMs, netns, and nft state.
	if err := mgr.Reconcile(context.Background()); err != nil {
		logger.Error("registry reconcile failed", "err", err)
		return 1
	}

	// sandboxes_active is a pull-model gauge: read the live count at
	// scrape time so it can't drift from reality across creates/deletes/
	// reconcile.
	mx.SetActiveSandboxSource(func() int { return len(mgr.List()) })

	srv, err := daemon.New(daemon.Config{
		Manager:    mgr,
		Addr:       *addr,
		Logger:     logger,
		Metrics:    mx,
		TokenStore: tokens,
		TLSCert:    *tlsCert,
		TLSKey:     *tlsKey,
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

// discoverProfiles scans dir for `<name>.ext4` images and returns a
// profile-name → absolute-path map. The basename (minus .ext4) is the
// profile name, so `python-3.12.ext4` yields profile "python-3.12".
// Symlinks are resolved, so a `node.ext4 -> node-22.ext4` alias produces
// a "node" profile pointing at the real image; a broken symlink is an
// error surfaced at startup rather than a confusing failure at create.
func discoverProfiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	profiles := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".ext4") {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", name, err)
		}
		profiles[strings.TrimSuffix(name, ".ext4")] = resolved
	}
	return profiles, nil
}

// isLoopbackAddr reports whether a listen address binds only loopback (so
// auth/TLS is optional). Empty host / 0.0.0.0 / any routable IP is
// non-loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseTokenArgs pulls the `--token-file`/`--name` flags out of args from
// any position (Go's flag package stops at the first positional, which
// makes `token revoke <id> --token-file X` silently ignore the flag), and
// returns the remaining positionals.
func parseTokenArgs(args []string) (tokenFile, name string, positionals []string) {
	tokenFile = defaultTokenFile
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--token-file" && i+1 < len(args):
			tokenFile, i = args[i+1], i+1
		case strings.HasPrefix(a, "--token-file="):
			tokenFile = strings.TrimPrefix(a, "--token-file=")
		case a == "--name" && i+1 < len(args):
			name, i = args[i+1], i+1
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		default:
			positionals = append(positionals, a)
		}
	}
	return tokenFile, name, positionals
}

// runDaemonToken handles `crucible daemon token <add|list|revoke>` — the
// operator-side management of the daemon's API keys. It edits the token
// file directly; a running daemon picks up changes without a restart.
func runDaemonToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: crucible daemon token <add|list|revoke> [--token-file PATH] [--name NAME] [id...]")
		return 2
	}
	sub := args[0]
	tokenFile, name, positionals := parseTokenArgs(args[1:])

	switch sub {
	case "add":
		if err := os.MkdirAll(filepath.Dir(tokenFile), 0o750); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: create token dir: %v\n", err)
			return 2
		}
		raw, e, err := tokenstore.Add(tokenFile, name)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "key created (id %s). Copy it now — it is not shown again:\n\n  %s\n\n", e.ID, raw)
		return 0

	case "list":
		entries, err := tokenstore.List(tokenFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		if len(entries) == 0 {
			_, _ = fmt.Fprintln(stdout, "no API keys")
			return 0
		}
		for _, e := range entries {
			label := e.Name
			if label == "" {
				label = "-"
			}
			_, _ = fmt.Fprintf(stdout, "%s  %-20s  %s\n", e.ID, label, e.CreatedAt.Format(time.RFC3339))
		}
		return 0

	case "revoke":
		if len(positionals) == 0 {
			_, _ = fmt.Fprintln(stderr, "usage: crucible daemon token revoke <id>...")
			return 2
		}
		for _, id := range positionals {
			ok, err := tokenstore.Revoke(tokenFile, id)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
				return 2
			}
			if !ok {
				_, _ = fmt.Fprintf(stderr, "no such key id %q\n", id)
				return 2
			}
			_, _ = fmt.Fprintf(stdout, "revoked %s\n", id)
		}
		return 0

	default:
		_, _ = fmt.Fprintf(stderr, "unknown token subcommand %q (want add|list|revoke)\n", sub)
		return 2
	}
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
