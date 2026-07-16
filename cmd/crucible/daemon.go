//go:build linux

// The daemon needs KVM + Firecracker + jailer, so its implementation is
// Linux-only. This file is the *sole* importer of the Linux-only daemon
// packages (sandbox/runner/jailer/network/oci/fsutil/…); gating it here is
// what lets the client (CLI/TUI/`mcp serve`) cross-compile to macOS and
// Windows. The non-Linux build gets the stub runDaemon in daemon_stub.go.
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
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentbin"
	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/daemon"
	"github.com/gnana997/crucible/internal/fsutil"
	"github.com/gnana997/crucible/internal/guestscrape"
	"github.com/gnana997/crucible/internal/ingress"
	"github.com/gnana997/crucible/internal/jailer"
	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/metrics"
	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/registryauth"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/secretstore"
	"github.com/gnana997/crucible/internal/telemetry"
	"github.com/gnana997/crucible/internal/tlscert"
	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// defaultTokenFile is where the daemon's API-key store lives by default.
const defaultTokenFile = "/var/lib/crucible/tokens.json"

// sandboxGuestIP adapts the sandbox Manager to ingress.InstanceLookup: it maps
// an app instance's sandbox id to its guest IP for the proxy to dial.
type sandboxGuestIP struct{ mgr *sandbox.Manager }

func (s sandboxGuestIP) GuestIP(instanceID string) (string, bool) {
	// Routable returns false for an asleep instance, so the resolver's cache
	// falls through to a fresh lookup (→ ErrAsleep) rather than routing to a
	// slept, VMM-stopped guest.
	return s.mgr.Routable(instanceID)
}

// internalAuthorizer implements ingress.CallerAuthorizer for app→app networking:
// it maps a source guest IP to its owning app (whichever app's current instance
// holds that IP) and checks the caller's can_call grant. Default-deny — an
// unrecognized source IP or a missing grant is not authorized.
type internalAuthorizer struct {
	apps *app.Manager
	ips  sandboxGuestIP // instance → guest IP
}

func (a internalAuthorizer) AuthorizeCall(callerIP, targetApp string) (string, bool) {
	caller, ok := a.appForGuestIP(callerIP)
	if !ok {
		return "", false
	}
	return caller, a.apps.CanCall(caller, targetApp)
}

func (a internalAuthorizer) appForGuestIP(ip string) (string, bool) {
	apps, err := a.apps.List()
	if err != nil {
		return "", false
	}
	for _, ap := range apps {
		if ap.Status != nil && ap.Status.InstanceID != "" {
			if gip, ok := a.ips.GuestIP(ap.Status.InstanceID); ok && gip == ip {
				return ap.Name, true
			}
		}
	}
	return "", false
}

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
// internalNetworkZone is the DNS suffix apps use to reach each other
// (<app>.internal) when --internal-networking is enabled (v0.5.1).
const internalNetworkZone = "internal"

// appLister is the app-manager method the scrape adapter needs (an interface so
// the filtering is unit-testable).
type appLister interface {
	List() ([]api.AppResponse, error)
}

// appScrapeTargets adapts the app manager to guestscrape.TargetSource: the apps
// that configured a --metrics-port, each with its current instance.
type appScrapeTargets struct{ apps appLister }

func (a appScrapeTargets) Targets() []guestscrape.Target {
	list, err := a.apps.List()
	if err != nil {
		return nil
	}
	out := make([]guestscrape.Target, 0, len(list))
	for _, ap := range list {
		if ap.MetricsPort <= 0 || ap.Status == nil || ap.Status.InstanceID == "" {
			continue
		}
		out = append(out, guestscrape.Target{
			App:      ap.Name,
			Instance: ap.Status.InstanceID,
			Port:     ap.MetricsPort,
			Path:     ap.MetricsPath,
		})
	}
	return out
}

// The return value is the exit code for the parent main().
func runDaemon(args []string, stdout, stderr io.Writer) int {
	// `crucible daemon token …` manages the API-key store (not the daemon).
	if len(args) > 0 && args[0] == "token" {
		return runDaemonToken(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("crucible daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr      = fs.String("listen", "127.0.0.1:7878", "HTTP listen address")
		fcBin     = fs.String("firecracker-bin", "", "path to the firecracker binary (required)")
		kernel    = fs.String("kernel", "", "path to the guest kernel image — uncompressed vmlinux (required)")
		rootfs    = fs.String("rootfs", "", "path to the guest root filesystem image (required; the default when a create request names no profile)")
		rootfsDir = fs.String("rootfs-dir", "", "directory of pre-baked <profile>.ext4 images; a create request's `profile` field selects one by basename (e.g. python-3.12.ext4 → profile \"python-3.12\")")
		workBase  = fs.String("work-base", "/tmp/crucible/run", "directory where per-sandbox workdirs are created")
		logFormat = fs.String("log-format", "text", "log format: text|json")
		logLevel  = fs.String("log-level", "info", "log level: debug|info|warn|error")
		// Observability (v0.5.4): telemetry identity + Go pprof. Exporters
		// (Prometheus /metrics is always on; OTLP arrives in a later milestone)
		// share this identity. OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES are
		// honored when the flag is unset.
		otelService  = fs.String("otel-service-name", "", "service.name for exported telemetry (default: $OTEL_SERVICE_NAME, else \"crucible\")")
		pprofListen  = fs.String("pprof-listen", "", "serve Go net/http/pprof on this address for daemon profiling; empty = off. Exposes process memory — bind loopback (e.g. 127.0.0.1:6060) or protect the port")
		otlpEndpoint = fs.String("otlp-endpoint", "", "OTLP endpoint to push metrics to (e.g. http://collector:4317); empty = off. Honors OTEL_EXPORTER_OTLP_ENDPOINT")
		otlpProtocol = fs.String("otlp-protocol", "", "OTLP protocol: grpc (default) or http")
		otlpHeaders  = fs.String("otlp-headers", "", "OTLP headers as k=v,k=v (auth/routing); also OTEL_EXPORTER_OTLP_HEADERS")
		otlpInsecure = fs.Bool("otlp-insecure", false, "use plaintext (no TLS) for the OTLP exporter")
		otlpLogs     = fs.Bool("otlp-logs", true, "export app logs over OTLP when --otlp-endpoint is set (requires --log-dir)")
		// Guest metrics scrape: fold an app's own Prometheus endpoint (a
		// postgres_exporter, redis_exporter, or the app itself, via `app create
		// --metrics-port`) into the daemon /metrics + OTLP. Awake instances only.
		scrapeInterval  = fs.Duration("guest-scrape-interval", 15*time.Second, "how often to scrape apps' --metrics-port endpoints")
		scrapeTimeout   = fs.Duration("guest-scrape-timeout", 5*time.Second, "per-scrape timeout for a guest metrics endpoint")
		scrapeMaxBody   = fs.Int64("guest-scrape-max-body", 1<<20, "max bytes read from a guest metrics endpoint per scrape")
		scrapeMaxSeries = fs.Int("guest-scrape-max-series", 2000, "max series accepted from a guest metrics endpoint per scrape")
		drainStr        = fs.String("drain-timeout", "30s", "max wallclock to wait for in-flight requests + sandbox drain on shutdown")
		noWaitAgent     = fs.Bool("no-wait-for-agent", false, "skip guest agent readiness polling on create (dev-only; needed when rootfs has no crucible-agent)")
		agentTimeout    = fs.String("agent-ready-timeout", "15s", "max wait for guest agent /healthz on create (ignored when --no-wait-for-agent)")
		// Jailer flags: when --jailer-bin is set, the daemon wraps every
		// firecracker instance in its own jailer chroot + mount/pid
		// namespace + cgroup v2 slice, and drops to --jail-uid/--jail-gid
		// before exec. Requires the daemon to run as root.
		jailerBin  = fs.String("jailer-bin", "", "path to jailer binary; when set, run firecracker under jailer (requires root)")
		chrootBase = fs.String("chroot-base", "/srv/jailer", "parent dir for per-VM jailer chroots (used only when --jailer-bin is set)")
		jailUID    = fs.Uint("jail-uid", 10000, "unprivileged uid jailer drops to before exec'ing firecracker")
		jailGID    = fs.Uint("jail-gid", defaultJailGID(), "unprivileged gid jailer drops to before exec'ing firecracker (defaults to the kvm group so the jailed firecracker can open /dev/kvm)")
		volumeDir  = fs.String("volume-dir", "", "directory for persistent volume backing files; enables --volume when set (must be on the same filesystem as --chroot-base so volumes hardlink into the jail)")
		volumeSize = fs.Int64("volume-default-size", 2<<30, "size in bytes a volume's backing file is created at on first use (2 GiB default; per-volume sizing lands in a later release)")

		volumeEncrypt    = fs.Bool("volume-encrypt", false, "encrypt new volumes at rest with per-volume LUKS keys (needs a master key via --volume-encrypt-key-file or CRUCIBLE_VOLUME_KEY); existing volumes are unaffected")
		volumeKeyFile    = fs.String("volume-encrypt-key-file", "", "file holding the base64 AES-256 master key (id `default`) that wraps per-volume encryption keys; generated 0600 on first use if absent, overridden by CRUCIBLE_VOLUME_KEY. Enables encrypted volumes + `volume shred`.")
		volumeKeyDir     = fs.String("volume-key-dir", "", "optional directory of additional base64 AES-256 keys as <id>.key files (id = filename); also read from CRUCIBLE_VOLUME_KEY_<ID> env vars. Lets volumes use more than one key and supports key rotation")
		volumeDefaultKey = fs.String("volume-default-key", "default", "keyring id that wraps NEW encrypted volumes (a per-volume override is possible at create)")
		backupDir        = fs.String("backup-dir", "", "directory for volume backups (default <volume-dir>/backups); point at another disk/mount for off-host durability. Backups reflink (O(1)) only when this shares the volume-dir filesystem, else a full copy")
		// cgroupQuotas sizes host-side cgroup v2 limits (cpu.max/memory.max/
		// pids.max) for each sandbox's VMM from its vCPU/memory request.
		// Only takes effect under jailer mode; the direct-exec runner has
		// no cgroup to write. "off" disables the limits.
		cgroupQuotas = fs.String("cgroup-quotas", "derive", "host cgroup v2 limits per sandbox VMM (jailer mode): derive|off")
		// maxFork bounds how many sandboxes a single fork request may create,
		// protecting the daemon from fan-out alone. 0 uses the built-in
		// default (64). A scoped token's own max_fork can only tighten this.
		maxFork = fs.Int("max-fork", envInt("CRUCIBLE_MAX_FORK", 0), "max sandboxes a single fork request may create (0 = built-in default of 64); env CRUCIBLE_MAX_FORK")
		// wakeMinFree refuses to wake a slept app when host MemAvailable is below
		// this floor, so scale-to-zero wakes can't drive the host into OOM. 0
		// disables the check.
		wakeMinFree = fs.Int("wake-min-free-mib", envInt("CRUCIBLE_WAKE_MIN_FREE_MIB", 256), "refuse to wake a slept app when host MemAvailable is below this many MiB (0 = disabled); env CRUCIBLE_WAKE_MIN_FREE_MIB")
		// sleepMinFreeDisk refuses to sleep (snapshot) an app when free disk under
		// --work-base is below this floor, so a fleet writing memory files can't
		// fill the disk. 0 disables the check.
		sleepMinFreeDisk = fs.Int("sleep-min-free-disk-mib", envInt("CRUCIBLE_SLEEP_MIN_FREE_DISK_MIB", 1024), "refuse to sleep (snapshot) an app when free disk under --work-base is below this many MiB (0 = disabled); env CRUCIBLE_SLEEP_MIN_FREE_DISK_MIB")
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
		// OCI image cache. When --image-dir is set, the daemon serves
		// /images (pull, import, ls, rm). Must live outside --work-base.
		// The injected guest agent comes from --agent-bin, else the
		// embedded copy (make build); without either, image support is
		// refused at startup.
		imageDir = fs.String("image-dir", "", "directory for the converted OCI image cache; enables /images when set (must be outside --work-base)")
		agentBin = fs.String("agent-bin", "", "path to the crucible-agent binary injected into converted images (overrides the embedded copy)")
		// Durable per-sandbox logs. Persists service output + exec activity
		// so `crucible logs` works and survives the sandbox. Kept outside
		// --work-base so the reconcile sweep can't reap it. Empty disables it.
		logDir = fs.String("log-dir", "/var/lib/crucible/logs", "directory for durable per-sandbox logs (service output + exec activity); empty disables `crucible logs`")
		// Durable app control-plane store (v0.4). Kept outside --work-base
		// (like --log-dir) so the sandbox reconcile sweep can't reap it.
		// Enables the /apps routes + reconcile loop; empty disables apps.
		appDB = fs.String("app-db", "/var/lib/crucible/apps.db", "bbolt file for durable apps; enables /apps + the reconcile loop when set (must be outside --work-base)")

		// Private-registry credentials (v0.4.4): pull authenticated images.
		registryStore  = fs.String("registry-store", "/var/lib/crucible/registry.json", "credential store for private-registry pulls (`crucible registry login`); enables /registry/credentials. Empty disables it (pulls stay anonymous).")
		secretsKeyFile = fs.String("secrets-key-file", "", "file holding the base64 AES-256 master key that encrypts secret bundles; enables /secrets. Generated 0600 on first use if absent. Overridden by CRUCIBLE_SECRETS_KEY. Empty (and no env key) disables secrets.")
		secretsDB      = fs.String("secrets-db", "/var/lib/crucible/secrets.db", "bbolt file for encrypted secret bundles (used only when a master key is configured)")

		// Ingress proxy (v0.4.2): route inbound traffic to an app by name.
		proxyListen    = fs.String("proxy-listen", "", "ingress proxy HTTP listen address (e.g. :80): routes by Host header to an app's current instance. Requires --app-db. Empty disables the HTTP proxy.")
		proxyTLSListen = fs.String("proxy-tls-listen", "", "ingress proxy TLS listen address (e.g. :443): terminates TLS for apps (default) or SNI-passthrough per app. Empty disables it.")
		// TLS termination for app HTTPS (v0.7.0). Setting --acme-email (ACME/Let's
		// Encrypt, automatic certs) OR --cert-dir (manual certs / storage) enables
		// termination on --proxy-tls-listen; with neither, :443 stays passthrough-only.
		acmeEmail   = fs.String("acme-email", "", "ACME account email — enables automatic HTTPS (Let's Encrypt) for app domains on --proxy-tls-listen. Empty = no ACME.")
		acmeCA      = fs.String("acme-ca", "production", "ACME CA: production | staging (Let's Encrypt), used when --acme-email is set")
		acmeCAURL   = fs.String("acme-ca-url", "", "override the ACME directory URL (e.g. a Pebble/private-CA endpoint); takes precedence over --acme-ca")
		acmeCARoot  = fs.String("acme-ca-root", "", "PEM file of root CA(s) to trust for the ACME server (a private/test CA whose endpoint isn't publicly trusted, e.g. Pebble); empty = system roots")
		certDir     = fs.String("cert-dir", "", "directory for TLS certs, keys, and ACME state (default /var/lib/crucible/certs when TLS termination is enabled)")
		proxyDomain = fs.String("proxy-domain", "", "base domain for name routing: <app>.<domain> routes to the app. Empty means the request Host IS the app name.")
		// Persistent usage metrics: durable per-app usage counters (compute,
		// memory, requests, storage) that survive a daemon restart. This is the
		// accrual/flush cadence; it also bounds loss on an unclean crash.
		usageInterval = fs.Duration("usage-interval", 60*time.Second, "cadence for accruing/persisting per-app usage metrics so they survive a daemon restart")
		eventsBuffer  = fs.Int("events-buffer", 1024, "size of the in-memory app lifecycle event ring served by GET /events")
		// App→app service networking (v0.5.1, experimental). Off by default:
		// reachability is default-deny — an app reaches a peer only if its spec
		// grants it (`app create --can-call <peer>`); ungranted calls get
		// NXDOMAIN at DNS / 403 at the proxy.
		internalNet  = fs.Bool("internal-networking", false, "EXPERIMENTAL: let apps reach each other by name (<app>.internal) through the ingress proxy VIP. Requires --network-egress-iface + --app-db + the proxy. Default-deny: a peer is reachable only via `app create --can-call <peer>`.")
		internalPort = fs.Int("internal-proxy-port", 80, "TCP port the app→app (<app>.internal) ingress VIP listens on, bound to the DNS anycast; guests reach peers at http://<app>.internal[:port]/")
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
		// First kill any live VMM processes left running by a previous daemon
		// that was killed without clean shutdown (e.g. kill -9). This PID-driven
		// sweep is scoped to our chroot-base and catches even a VM whose chroot
		// dir is already gone — which the directory-driven ReapOrphans cannot see.
		if killed := jailer.KillLiveOrphans(*chrootBase); len(killed) > 0 {
			logger.Info("killed live orphan VMs from previous run", "count", len(killed), "ids", killed)
		}
		if reaped, err := jailer.ReapOrphans(*chrootBase, *fcBin); err != nil {
			logger.Warn("orphan reap failed (continuing)", "err", err)
		} else if len(reaped) > 0 {
			logger.Info("reaped orphan chroots from previous run", "count", len(reaped), "ids", reaped)
		}
		// Finally, sweep any empty per-VM cgroup dirs left behind — the reaps
		// above are chroot-driven and can't see a cgroup whose chroot is gone.
		if reaped := jailer.ReapOrphanCgroups(*fcBin); len(reaped) > 0 {
			logger.Info("reaped orphan cgroups from previous run", "count", len(reaped))
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

	// Declared before the network manager so the DNS proxy's app→app authorizer
	// (below) can close over them; both are assigned later (appMgr when durable
	// apps start, internalAuth when the proxy starts), and the closure is only
	// invoked at query time (after that), nil-checking to fail closed in the
	// interim. internalAuth is the SAME (source-IP-keyed) authorizer the ingress
	// proxy uses, so the DNS and proxy layers agree on the caller→app mapping.
	var appMgr *app.Manager
	var internalAuth *internalAuthorizer
	var netMgr *network.Manager
	if *netEgressIface != "" && *jailerBin != "" {
		subnetPool, perr := netip.ParsePrefix(*netSubnetPool)
		if perr != nil {
			_, _ = fmt.Fprintf(stderr, "error: --network-subnet-pool: %v\n", perr)
			return 2
		}
		// App→app networking opens the guest→VIP nft allow + the internal DNS zone
		// only when explicitly enabled (see --internal-networking) AND the ingress
		// proxy is running (otherwise nothing would answer at the VIP).
		internalProxyPort := 0
		internalZone := ""
		if *internalNet && (*proxyListen != "" || *proxyTLSListen != "") {
			internalProxyPort = *internalPort
			internalZone = internalNetworkZone
		}
		nmgr, nerr := network.Start(context.Background(), network.ManagerConfig{
			SubnetPool:        subnetPool,
			DNSAnycast:        network.DefaultDNSAnycast,
			EgressIface:       *netEgressIface,
			DNSUpstream:       *dnsUpstream,
			InternalProxyPort: internalProxyPort,
			InternalZone:      internalZone,
			// App→app DNS authorization (default-deny), via the same source-IP-keyed
			// authorizer the ingress proxy uses. nil (proxy not up) / unknown source
			// / missing grant → deny (NXDOMAIN at the DNS layer).
			InternalAuthz: func(callerIP, target string) bool {
				if internalAuth == nil {
					return false
				}
				_, ok := internalAuth.AuthorizeCall(callerIP, target)
				return ok
			},
			Logger: logger,
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

	// Telemetry seam (v0.5.4): the daemon's exported-signal identity + OTLP metric
	// export. Prometheus /metrics is separate and always on; OTLP bridges the same
	// registry and pushes it when --otlp-endpoint (or OTEL_* env) is set. Failure
	// to set up OTLP is logged and skipped — the daemon still starts.
	tele := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  *otelService,
		Logger:       logger,
		OTLPEndpoint: *otlpEndpoint,
		OTLPProtocol: *otlpProtocol,
		OTLPHeaders:  *otlpHeaders,
		OTLPInsecure: *otlpInsecure,
		Gatherer:     mx.Gatherer(),
	})

	// Go pprof (v0.5.4 J9 slice): off unless --pprof-listen is set.
	var pprofSrv *http.Server
	if *pprofListen != "" {
		if !telemetry.IsLoopbackAddr(*pprofListen) {
			logger.Warn("pprof listening on a non-loopback address — it exposes process memory; protect the port", "addr", *pprofListen)
		}
		pprofSrv = telemetry.StartPprof(*pprofListen, func(err error) { logger.Error("pprof server failed", "err", err) })
		logger.Info("pprof enabled", "addr", *pprofListen)
	}

	// Persistent volumes (optional). Backing files are chowned to the user
	// firecracker runs as: the jailer uid/gid under jailer, or the daemon's
	// own uid/gid for direct-exec. Same-filesystem-as-chroot is required so
	// volumes hardlink into the jail (volume.Manager / jailer.Stage enforce
	// it at attach time).
	var volMgr *volume.Manager
	var reloadVolumeKeys func() error
	if *volumeDir != "" {
		vuid, vgid := os.Getuid(), os.Getgid()
		if *jailerBin != "" {
			vuid, vgid = int(*jailUID), int(*jailGID)
		}
		hostID, _ := os.Hostname()
		vm, verr := volume.NewManager(*volumeDir, *volumeSize, hostID, vuid, vgid)
		if verr != nil {
			_, _ = fmt.Fprintf(stderr, "error: init volume storage: %v\n", verr)
			return 1
		}
		volMgr = vm
		volMgr.SetBackupDir(*backupDir)
		defer func() { _ = volMgr.Close() }()
		logger.Info("volumes enabled", "dir", *volumeDir, "default_size_bytes", *volumeSize, "host_id", hostID)

		// Per-volume encryption at rest (opt-in). Assemble the keyring (the default
		// key + any additional keys) and EnableEncryption — which also reaps mapper
		// devices a crashed daemon left open, so it must run here at startup, before
		// the HTTP listener opens and any volume is attached.
		ring, generated, kerr := buildVolumeKeyring(*volumeKeyFile, *volumeKeyDir)
		if kerr != nil {
			_, _ = fmt.Fprintf(stderr, "error: volume encryption keyring: %v\n", kerr)
			return 2
		}
		if len(ring) > 0 {
			if generated {
				logger.Warn("generated a new volume encryption master key — BACK IT UP; losing it loses every encrypted volume", "file", *volumeKeyFile)
			}
			if eerr := volMgr.EnableEncryption(ring, *volumeDefaultKey, *volumeEncrypt); eerr != nil {
				_, _ = fmt.Fprintf(stderr, "error: enable volume encryption: %v\n", eerr)
				return 2
			}
			volMgr.SetAuditLogger(logger.With("component", "volume_key_audit"))
			logger.Info("volume encryption enabled", "keys", len(ring), "default_key", *volumeDefaultKey, "default_encrypt", *volumeEncrypt)
			// A reloader for POST /volumes/keys/reload: re-read the key sources and
			// swap the keyring in without a restart.
			vkFile, vkDir := *volumeKeyFile, *volumeKeyDir
			reloadVolumeKeys = func() error {
				ring, _, err := buildVolumeKeyring(vkFile, vkDir)
				if err != nil {
					return err
				}
				return volMgr.ReloadKeyring(ring)
			}
			// Volume encryption protects the data volume, but a slept app's memory
			// snapshot (which can hold cached rows) is written under --work-base — in
			// the clear unless that sits on an encrypted filesystem. Advise when we
			// can positively see it is plaintext; stay silent when unsure.
			if fsutil.PathAtRest(*workBase) == fsutil.AtRestPlaintext {
				logger.Warn("volume encryption is on but --work-base is on unencrypted storage: a slept app's memory snapshot (cached rows, buffers) is written to disk in the clear. Put --work-base on a dm-crypt/LUKS filesystem for full encryption at rest — see docs/encryption.md", "work_base", *workBase)
			}
		} else if *volumeEncrypt {
			_, _ = fmt.Fprintf(stderr, "error: --volume-encrypt requires a master key (--volume-encrypt-key-file, CRUCIBLE_VOLUME_KEY, or --volume-key-dir)\n")
			return 2
		}
	}

	mgrCfg := sandbox.ManagerConfig{
		Runner:            r,
		VolumeManager:     volMgr,
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
		QuotaPolicy:         quotaPolicy,
		MaxForkCount:        *maxFork,
		WakeMinFreeMiB:      *wakeMinFree,
		SleepMinFreeDiskMiB: *sleepMinFreeDisk,
	}
	if netMgr != nil {
		mgrCfg.Network = daemon.NewNetworkAdapter(netMgr)
		// Host port publish rides on the network layer (it forwards to
		// the guest IP), so enable it exactly when networking is on.
		mgrCfg.PortPublisher = daemon.NewPortPublisher(logger)
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
	mx.SetSnapshotSource(mgr.SnapshotCount)
	// Disk gauges (same pull model): what snapshots, volumes, and backups
	// actually occupy, so scale-to-zero density is visible as disk, not just
	// counts. Volume/backup series exist only when volumes are enabled.
	var volDisk, bakDisk func() int64
	if volMgr != nil {
		volDisk, bakDisk = volMgr.DiskBytes, volMgr.BackupDiskBytes
	}
	mx.SetDiskSources(mgr.SnapshotDiskBytes, volDisk, bakDisk)

	// OCI image store (optional). Enabled by --image-dir; the injected
	// Private-registry credential store (optional). Empty path disables it —
	// pulls stay anonymous. Built before the image store so its keychain can be
	// wired into pulls.
	var regStore *registryauth.Store
	if *registryStore != "" {
		regStore = registryauth.Open(*registryStore)
	}

	// Encrypted secret store (v0.7.4). Opt-in: only when a master key is
	// configured (a key file or CRUCIBLE_SECRETS_KEY). No key ⇒ secrets stay off
	// (and /secrets answers 501) — no silent plaintext fallback.
	var secStore *secretstore.Store
	if key, generated, kerr := secretstore.LoadMasterKey(*secretsKeyFile); kerr != nil {
		_, _ = fmt.Fprintf(stderr, "error: secrets key: %v\n", kerr)
		return 2
	} else if key != nil {
		if generated {
			logger.Warn("generated a new secrets master key — BACK IT UP; losing it loses every secret", "file", *secretsKeyFile)
		}
		st, serr := secretstore.Open(*secretsDB, key)
		if serr != nil {
			_, _ = fmt.Fprintf(stderr, "error: --secrets-db: %v\n", serr)
			return 2
		}
		secStore = st
		defer func() { _ = secStore.Close() }()
		logger.Info("secrets enabled", "db", *secretsDB)
	}

	// agent comes from --agent-bin or the embedded copy.
	var imageStore daemon.ImageStore
	if *imageDir != "" {
		store, serr := buildImageStore(context.Background(), *imageDir, *workBase, *agentBin, regStore, logger)
		if serr != nil {
			_, _ = fmt.Fprintf(stderr, "error: --image-dir: %v\n", serr)
			return 2
		}
		imageStore = store
	}

	// Durable log store (optional). Failure to create the dir degrades to
	// no durable logs rather than refusing to start — logs are best-effort.
	var logStore *logstore.Store
	if *logDir != "" {
		ls, lerr := logstore.New(*logDir)
		if lerr != nil {
			logger.Warn("durable logs disabled", "log_dir", *logDir, "err", lerr)
		} else {
			logStore = ls
			logger.Info("durable logs enabled", "log_dir", *logDir)
		}
	}
	// Stream app logs over OTLP (v0.5.4) — no-op unless --otlp-endpoint is
	// set; taps the log store's best-effort fanout so it never blocks the app.
	if *otlpLogs && logStore != nil {
		tele.StartLogExport(context.Background(), logStore)
	}

	srv, err := daemon.New(daemon.Config{
		Manager:          mgr,
		Addr:             *addr,
		Logger:           logger,
		Metrics:          mx,
		TokenStore:       tokens,
		TLSCert:          *tlsCert,
		TLSKey:           *tlsKey,
		Images:           imageStore,
		LogStore:         logStore,
		RegistryStore:    regStore,
		SecretStore:      secStore,
		Volumes:          volMgr,
		ReloadVolumeKeys: reloadVolumeKeys,
	})
	if err != nil {
		logger.Error("daemon init failed", "err", err)
		return 1
	}

	// Durable app control plane (v0.4, optional). Opened after the sandbox
	// reconcile above has reaped the previous run's instances, so the
	// app reconciler's initial pass boots fresh instances from persisted
	// desired state — this is how an app survives a daemon restart.
	// (appMgr is declared earlier so the DNS authorizer can close over it.)
	var appStore *app.Store
	var activityTracker *ingress.ActivityTracker
	var wakeForwarders *ingress.WakeForwarderSet
	if *appDB != "" {
		as, aerr := app.Open(*appDB)
		if aerr != nil {
			logger.Warn("durable apps disabled", "app_db", *appDB, "err", aerr)
		} else {
			appStore = as
			appMgr = app.NewManager(as, srv.NewAppInstantiator(), logger)
			appMgr.SetEventsBuffer(*eventsBuffer) // before any subscriber attaches
			srv.SetAppManager(appMgr)
			// Wire request-activity tracking + the L4 waking forwarders BEFORE Start
			// so the idle monitor and wake-on-connect path are live from the first
			// reconcile. Both work whether or not the HTTP proxy is enabled — a TCP
			// app (postgres, redis) is reached through a published port, invisible to
			// the L7 proxy, so its wake + idle detection live here, not in the proxy.
			activityTracker = ingress.NewActivityTracker()
			appMgr.SetActivitySource(activityTracker)
			// The forwarder resolves an app by name → its current instance; domain /
			// internal-zone are irrelevant to ResolveName, so a bare resolver suffices.
			l4Resolver := ingress.NewResolver(appMgr, sandboxGuestIP{mgr}, "", "", time.Second)
			wakeForwarders = ingress.NewWakeForwarderSet(l4Resolver, appMgr, activityTracker, logger)
			appMgr.SetPortReconciler(wakeForwarders)
			// Persistent usage metrics: sample per-app volume storage from the
			// volume manager (nil ⇒ storage stays 0), and set the flush cadence.
			if volMgr != nil {
				appMgr.SetVolumeSizer(volMgr.VolumeDiskBytes)
			}
			// Per-app external egress bytes, read from the per-sandbox nft
			// counters once per usage tick (real sandbox-id keyed).
			appMgr.SetEgressSource(mgr.EgressByteMap)
			appMgr.SetUsageInterval(*usageInterval)
			// Push app lifecycle events over OTLP too (no-op unless OTLP is on);
			// GET /events serves them regardless.
			tele.StartEventExport(context.Background(), appMgr.Events())
			if serr := appMgr.Start(context.Background()); serr != nil {
				logger.Warn("app reconciler start failed", "err", serr)
			} else {
				logger.Info("durable apps enabled", "app_db", *appDB)
			}
			// Guest metrics scrape (v0.9.0): fold an app's own Prometheus endpoint
			// into /metrics + OTLP. Awake instances only (resolver = the sandbox
			// manager's Routable). Always on — a no-op until an app sets --metrics-port.
			scr := guestscrape.New(appScrapeTargets{appMgr}, mgr, guestscrape.Options{
				Interval:  *scrapeInterval,
				Timeout:   *scrapeTimeout,
				MaxBody:   *scrapeMaxBody,
				MaxSeries: *scrapeMaxSeries,
				Logger:    logger.With("component", "guest_scrape"),
			})
			if rerr := mx.Register(scr.Collector()); rerr != nil {
				logger.Warn("guest metrics scrape collector not registered", "err", rerr)
			} else {
				go scr.Run(context.Background())
				logger.Info("guest metrics scrape enabled", "interval", *scrapeInterval)
			}
		}
	}

	// Per-app lifecycle metrics (v0.5.4): pull-model, read from the app
	// manager at scrape time so a deleted app simply stops being reported.
	if appMgr != nil {
		mx.SetAppStateSource(func() []metrics.AppState {
			apps, lerr := appMgr.List()
			if lerr != nil {
				return nil
			}
			out := make([]metrics.AppState, 0, len(apps))
			for _, a := range apps {
				st := metrics.AppState{Name: a.Name}
				if a.Status != nil {
					st.Phase = a.Status.Phase
					st.Replicas = a.Status.Replicas
					st.ReadyReplicas = a.Status.ReadyReplicas
					st.SleepCount = a.Status.SleepCount
					st.LastWakeLatencyMs = a.Status.LastWakeLatencyMs
				}
				out = append(out, st)
			}
			return out
		})
		// Persistent usage metrics: emit LIVE apps' cumulative counters (a deleted
		// app's retained record stays readable via GET /usage, but drops off /metrics).
		mx.SetUsageSource(func() []metrics.AppUsageStat {
			all := appMgr.AllUsage()
			out := make([]metrics.AppUsageStat, 0, len(all))
			for _, u := range all {
				if u.FinalizedAt != nil {
					continue // retained (deleted) — not a live series
				}
				out = append(out, metrics.AppUsageStat{
					Name:               u.AppName,
					ComputeVCPUSeconds: u.ComputeVCPUSeconds,
					MemoryMiBSeconds:   u.MemoryMiBSeconds,
					StorageGiBSeconds:  u.StorageGiBSeconds,
					Requests:           u.Requests,
					RequestsByCode:     u.RequestsByCode,
					EgressBytes:        u.EgressBytes,
				})
			}
			return out
		})
	}

	// Ingress proxy (v0.4.2): reach an app by name. Needs the app manager
	// (name → current instance) and the sandbox manager (instance → guest IP).
	var proxy *ingress.Proxy
	if *proxyListen != "" || *proxyTLSListen != "" {
		if appMgr == nil {
			logger.Warn("ingress proxy requested but durable apps are disabled; set --app-db", "proxy_listen", *proxyListen, "proxy_tls_listen", *proxyTLSListen)
		} else {
			// App→app (v0.5.1): the internal listener binds the DNS anycast VIP, so
			// it needs the network manager up (the anycast lives on its dummy iface).
			internalZone, internalListen := "", ""
			if *internalNet && netMgr != nil {
				internalZone = internalNetworkZone
				internalListen = net.JoinHostPort(network.DefaultDNSAnycast.String(), strconv.Itoa(*internalPort))
			} else if *internalNet {
				logger.Warn("internal-networking requested but network is disabled; app→app networking off (set --network-egress-iface + --jailer-bin)")
			}
			resolver := ingress.NewResolver(appMgr, sandboxGuestIP{mgr}, *proxyDomain, internalZone, time.Second)

			// TLS termination (v0.7.0): enabled when --acme-email or --cert-dir is
			// set. On-demand ACME issuance is gated to registered terminate-mode app
			// domains (resolver.TLSTerminate), so a stray SNI can't burn a cert.
			var certProvider ingress.CertProvider
			if *proxyTLSListen != "" && (*acmeEmail != "" || *certDir != "") {
				cd := *certDir
				if cd == "" {
					cd = "/var/lib/crucible/certs"
				}
				var caRootPEM []byte
				if *acmeCARoot != "" {
					b, rerr := os.ReadFile(*acmeCARoot)
					if rerr != nil {
						logger.Error("reading --acme-ca-root failed", "err", rerr)
						return 1
					}
					caRootPEM = b
				}
				tp, terr := tlscert.New(tlscert.Config{
					CertDir:   cd,
					Email:     *acmeEmail,
					CAURL:     *acmeCAURL,
					Staging:   *acmeCA == "staging",
					CARootPEM: caRootPEM,
					Allow:     resolver.TLSTerminate,
				})
				if terr != nil {
					logger.Error("TLS termination setup failed", "err", terr)
					return 1
				}
				defer tp.Close()
				certProvider = tp
				// Per-domain cert status for `GET /apps/{name}/domains?detail=1`
				// (state, expiry, last ACME error) + the app's generated name.
				srv.SetCertStatusSource(tp.Status, *proxyDomain)
				// app_cert_state{app,domain,state} + app_cert_not_after_seconds:
				// per-domain cert status for monitoring to alert on (expiry / failed).
				mx.SetCertStatusSource(func() []metrics.AppCertStat {
					apps, lerr := appMgr.List()
					if lerr != nil {
						return nil
					}
					var out []metrics.AppCertStat
					for _, a := range apps {
						mode := a.TLSMode
						if mode == "" {
							mode = "terminate"
						}
						domains := append([]string{}, a.Domains...)
						if *proxyDomain != "" {
							domains = append(domains, a.Name+"."+*proxyDomain)
						}
						for _, d := range domains {
							st := metrics.AppCertStat{App: a.Name, Domain: d}
							if mode == "passthrough" {
								st.State = "passthrough"
							} else {
								cs := tp.Status(d)
								st.State = cs.State
								if cs.NotAfter != nil {
									st.NotAfterUnix = cs.NotAfter.Unix()
								}
							}
							out = append(out, st)
						}
					}
					return out
				})
				// Pre-warm a cert when a custom domain is attached, so the first
				// live HTTPS request isn't delayed by issuance (best-effort).
				appMgr.SetOnDomainAdd(func(domain string) {
					if err := tp.Prewarm(context.Background(), domain); err != nil {
						logger.Warn("cert pre-warm failed; on-demand will cover it", "domain", domain, "err", err)
					}
				})
				// cert_expiry_seconds = soonest managed-cert expiry (alerting).
				// Large sentinel (~1yr) when no certs yet, so a "<7d" alert can't
				// fire falsely before the first issuance.
				mx.SetCertExpirySource(func() float64 {
					soonest := 365 * 24 * time.Hour
					for _, d := range appMgr.AllDomains() {
						if na, ok := tp.NotAfter(d); ok {
							if left := time.Until(na); left < soonest {
								soonest = left
							}
						}
					}
					return soonest.Seconds()
				})
				effCA := *acmeCA
				if *acmeCAURL != "" {
					effCA = *acmeCAURL // explicit override (private/test CA)
				}
				logger.Info("ingress TLS termination enabled", "cert_dir", cd, "acme", *acmeEmail != "", "ca", effCA)
			}
			var internalAuthz ingress.CallerAuthorizer
			if internalListen != "" {
				// The same authorizer instance backs both the proxy (per-request 403)
				// and the DNS proxy (NXDOMAIN), so the two layers can't disagree.
				internalAuth = &internalAuthorizer{apps: appMgr, ips: sandboxGuestIP{mgr}}
				internalAuthz = internalAuth
			}
			proxy = ingress.New(ingress.Config{
				Resolver:       resolver,
				HTTPListen:     *proxyListen,
				TLSListen:      *proxyTLSListen,
				InternalListen: internalListen,
				InternalAuthz:  internalAuthz,
				Certs:          certProvider,
				Logger:         logger,
				Waker:          appMgr,          // wake a slept app on the first request for it
				Activity:       activityTracker, // feed the idle monitor
				OnWake:         mx.ObserveWakeLatency,
				OnInternal:     mx.IncInternalRequest,
				OnRequest: func(app, code string, latency time.Duration, _ bool) {
					mx.ObserveAppRequest(app, code, latency)
					appMgr.RecordRequest(app, code) // durable per-app request count
				},
			})
			if perr := proxy.Start(); perr != nil {
				logger.Error("ingress proxy start failed", "err", perr)
				return 1
			}
			logger.Info("ingress proxy enabled", "http", *proxyListen, "tls", *proxyTLSListen, "domain", *proxyDomain, "internal", internalListen)
		}
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

	// GC the push-model per-app request series for deleted apps (the pull-model
	// gauges self-GC). Cheap sweep off the app list; the pull gauges are unaffected.
	if appMgr != nil {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					live := map[string]struct{}{}
					if apps, lerr := appMgr.List(); lerr == nil {
						for _, a := range apps {
							live[a.Name] = struct{}{}
						}
					}
					mx.SyncApps(live)
				}
			}
		}()
	}

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
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(drainCtx)
	}
	_ = tele.Shutdown(drainCtx)
	// Stop accepting new proxied traffic before the reconciler and sandbox
	// drain tear instances down.
	if proxy != nil {
		proxy.Stop(drainCtx)
	}
	// Close the app-scoped waking TCP forwarders too (free their host ports).
	if wakeForwarders != nil {
		wakeForwarders.Close()
	}
	// Stop the app reconcile loop before draining sandboxes so it doesn't
	// try to "heal" instances the drain is tearing down. Desired state
	// stays in the store for the next start to reconcile from.
	if appMgr != nil {
		appMgr.Stop()
	}
	mgr.Shutdown(drainCtx)
	if appStore != nil {
		_ = appStore.Close()
	}
	if logStore != nil {
		_ = logStore.Close()
	}
	logger.Info("crucible stopped")
	_ = stdout // reserved for future non-log output
	return 0
}

// buildVolumeKeyring assembles the per-volume encryption keyring: the default key
// (id "default", from CRUCIBLE_VOLUME_KEY or defaultKeyFile — generated on first
// use), plus any additional keys from CRUCIBLE_VOLUME_KEY_<ID> env vars and
// <id>.key files under keyDir. Returns the keyring, whether the default key was
// freshly generated, and any decode error. An empty keyring means encryption is
// off. Key material is never logged.
func buildVolumeKeyring(defaultKeyFile, keyDir string) (map[string][]byte, bool, error) {
	ring := map[string][]byte{}
	generated := false
	if key, gen, err := secretstore.LoadMasterKeyFrom("CRUCIBLE_VOLUME_KEY", defaultKeyFile); err != nil {
		return nil, false, err
	} else if key != nil {
		ring["default"] = key
		generated = gen
	}
	const envPrefix = "CRUCIBLE_VOLUME_KEY_"
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(name, envPrefix) || val == "" {
			continue
		}
		id := name[len(envPrefix):]
		if id == "" {
			continue
		}
		key, err := secretstore.DecodeKey(val)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", name, err)
		}
		ring[id] = key
	}
	if keyDir != "" {
		ents, err := os.ReadDir(keyDir)
		if err != nil {
			return nil, false, fmt.Errorf("--volume-key-dir: %w", err)
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".key") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(keyDir, e.Name()))
			if err != nil {
				return nil, false, fmt.Errorf("read key %s: %w", e.Name(), err)
			}
			key, err := secretstore.DecodeKey(string(b))
			if err != nil {
				return nil, false, fmt.Errorf("key %s: %w", e.Name(), err)
			}
			ring[strings.TrimSuffix(e.Name(), ".key")] = key
		}
	}
	return ring, generated, nil
}

// buildImageStore constructs the OCI image store: resolve the agent to
// inject (--agent-bin, else the embedded copy), probe the host mkfs for
// tarball support, and open the content-addressed cache. imageDir must
// not sit inside workBase — the sandbox reconcile sweep would reap it.
func buildImageStore(ctx context.Context, imageDir, workBase, agentBin string, regStore *registryauth.Store, logger *slog.Logger) (*oci.Store, error) {
	absImg, err := filepath.Abs(imageDir)
	if err != nil {
		return nil, err
	}
	absWork, err := filepath.Abs(workBase)
	if err != nil {
		return nil, err
	}
	if absImg == absWork || strings.HasPrefix(absImg+string(os.PathSeparator), absWork+string(os.PathSeparator)) {
		return nil, fmt.Errorf("must not be inside --work-base %q (the reconcile sweep would delete the image cache)", workBase)
	}

	agent, err := resolveAgentBinary(agentBin)
	if err != nil {
		return nil, err
	}

	mode := oci.ModeStaging
	if oci.ProbeTarballSupport(ctx) {
		mode = oci.ModePipe
	} else {
		logger.Warn("mkfs.ext4 lacks tarball support; using staging mode for image conversion (upgrade e2fsprogs to >=1.47.1 for the faster, more isolated path)")
	}
	// Feed private-registry credentials to pulls when a store is configured;
	// unknown registries fall back to anonymous, so public pulls are unaffected.
	var pullOpts []oci.PullOption
	if regStore != nil {
		pullOpts = append(pullOpts, oci.WithKeychain(regStore.Keychain()))
	}
	return oci.New(oci.StoreConfig{Dir: absImg, Agent: agent, Mode: mode, PullOptions: pullOpts, Logger: logger})
}

// resolveAgentBinary loads the agent to inject into images: the
// --agent-bin file if set, else the embedded copy. Errors when neither
// is available so image support fails loudly rather than baking a
// zero-byte agent.
func resolveAgentBinary(agentBin string) ([]byte, error) {
	if agentBin != "" {
		data, err := os.ReadFile(agentBin)
		if err != nil {
			return nil, fmt.Errorf("read --agent-bin: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("--agent-bin %q is empty", agentBin)
		}
		return data, nil
	}
	if len(agentbin.Binary) > 0 {
		return agentbin.Binary, nil
	}
	return nil, errors.New("no agent binary: this build has no embedded agent (build with `make build`) — set --agent-bin")
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

// defaultJailGID picks the gid the jailer drops firecracker to when --jail-gid
// isn't given. It prefers the "kvm" group so the jailed firecracker can open
// /dev/kvm (root:kvm, mode 660) out of the box — the alternative is a cryptic
// "creating KVM object: Permission denied" on the first sandbox. Hosts without
// a kvm group fall back to 10000; an explicit --jail-gid always wins.
func defaultJailGID() uint {
	if g, err := user.LookupGroup("kvm"); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil && gid > 0 {
			return uint(gid)
		}
	}
	return 10000
}

// envInt returns the integer value of the env var named key, or def when the
// var is unset or not a valid integer.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// tokenArgs holds the flags/positionals pulled from a `daemon token` invocation.
type tokenArgs struct {
	tokenFile   string
	name        string
	policyFile  string
	ttl         string
	positionals []string
}

// parseTokenArgs pulls the `--token-file`/`--name`/`--policy`/`--ttl` flags out
// of args from any position (Go's flag package stops at the first positional,
// which makes `token revoke <id> --token-file X` silently ignore the flag).
func parseTokenArgs(args []string) tokenArgs {
	ta := tokenArgs{tokenFile: defaultTokenFile}
	take := func(cur *string, i *int, a, flag string) bool {
		if a == flag && *i+1 < len(args) {
			*cur, *i = args[*i+1], *i+1
			return true
		}
		if strings.HasPrefix(a, flag+"=") {
			*cur = strings.TrimPrefix(a, flag+"=")
			return true
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case take(&ta.tokenFile, &i, a, "--token-file"):
		case take(&ta.name, &i, a, "--name"):
		case take(&ta.policyFile, &i, a, "--policy"):
		case take(&ta.ttl, &i, a, "--ttl"):
		default:
			ta.positionals = append(ta.positionals, a)
		}
	}
	return ta
}

// runDaemonToken handles `crucible daemon token <add|list|revoke>` — the
// operator-side management of the daemon's API keys. It edits the token
// file directly; a running daemon picks up changes without a restart.
func runDaemonToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: crucible daemon token <add|list|revoke> [--token-file PATH] [--name NAME] [--policy FILE] [--ttl DUR] [id...]")
		return 2
	}
	sub := args[0]
	ta := parseTokenArgs(args[1:])

	switch sub {
	case "add":
		if err := os.MkdirAll(filepath.Dir(ta.tokenFile), 0o750); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: create token dir: %v\n", err)
			return 2
		}
		opts, err := buildAddOptions(ta)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		raw, e, err := tokenstore.Add(ta.tokenFile, opts)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "key created (id %s, %s). Copy it now — it is not shown again:\n\n  %s\n\n", e.ID, describeScope(e), raw)
		return 0

	case "list":
		entries, err := tokenstore.List(ta.tokenFile)
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
			scope := "full"
			if e.Scoped() {
				scope = "scoped"
			}
			expiry := "never"
			if e.ExpiresAt != nil {
				expiry = e.ExpiresAt.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(stdout, "%s  %-20s  %-6s  expires:%s  %s\n",
				e.ID, label, scope, expiry, e.CreatedAt.Format(time.RFC3339))
		}
		return 0

	case "revoke":
		if len(ta.positionals) == 0 {
			_, _ = fmt.Fprintln(stderr, "usage: crucible daemon token revoke <id>...")
			return 2
		}
		for _, id := range ta.positionals {
			ok, err := tokenstore.Revoke(ta.tokenFile, id)
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

// buildAddOptions turns the parsed token flags into AddOptions, reading and
// validating the policy file (fail-closed: an invalid policy is an error, so no
// token is minted) and parsing the TTL.
func buildAddOptions(ta tokenArgs) (tokenstore.AddOptions, error) {
	opts := tokenstore.AddOptions{Name: ta.name}
	if ta.policyFile != "" {
		data, err := os.ReadFile(ta.policyFile)
		if err != nil {
			return opts, fmt.Errorf("read policy file: %w", err)
		}
		p, err := policy.ParseAndValidate(data)
		if err != nil {
			return opts, fmt.Errorf("invalid policy %s: %w", ta.policyFile, err)
		}
		opts.Policy = &p
	}
	if ta.ttl != "" {
		d, err := time.ParseDuration(ta.ttl)
		if err != nil {
			return opts, fmt.Errorf("invalid --ttl %q: %w", ta.ttl, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("--ttl must be positive, got %s", ta.ttl)
		}
		opts.TTL = d
	}
	return opts, nil
}

// describeScope is the one-line scope summary printed after minting a key.
func describeScope(e tokenstore.Entry) string {
	scope := "full access"
	if e.Scoped() {
		scope = "scoped"
	}
	if e.ExpiresAt != nil {
		return fmt.Sprintf("%s, expires %s", scope, e.ExpiresAt.Format(time.RFC3339))
	}
	return scope
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
