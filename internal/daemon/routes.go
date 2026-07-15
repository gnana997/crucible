package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// routes builds the http.ServeMux for this Server. Keeping it in its own
// method (rather than inlining in New) makes it trivial to test handlers
// in isolation or to replace routing later (e.g. a router library).
//
// Uses Go 1.22 method-aware patterns: `METHOD /path/{param}`. Unmatched
// methods on a known path yield 405 automatically.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /whoami", s.handleWhoami)
	// Metrics endpoint. When Config.Metrics is nil the handler is a 404,
	// so the route is safe to register unconditionally.
	mux.Handle("GET /metrics", s.cfg.Metrics.Handler())
	mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	mux.HandleFunc("GET /sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /sandboxes/{id}", s.handleGetSandbox)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDeleteSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.handleExecSandbox)
	// WebSocket variant of interactive exec — the cross-language
	// transport (see exec_ws.go). Gated as `exec`, not `read`.
	mux.HandleFunc("GET /sandboxes/{id}/exec", s.handleExecWS)
	mux.HandleFunc("POST /sandboxes/{id}/files", s.handlePutFiles)
	mux.HandleFunc("GET /sandboxes/{id}/files", s.handleGetFile)
	// Supervised-service API (experimental): proxies to the guest
	// agent's supervisor. Mutations are gated as `exec` operations,
	// reads as `read` — see operationFor.
	mux.HandleFunc("PUT /sandboxes/{id}/service", s.handleConfigureService)
	mux.HandleFunc("POST /sandboxes/{id}/service/start", s.handleServiceStart)
	mux.HandleFunc("POST /sandboxes/{id}/service/stop", s.handleServiceStop)
	mux.HandleFunc("POST /sandboxes/{id}/service/restart", s.handleServiceRestart)
	mux.HandleFunc("GET /sandboxes/{id}/service", s.handleServiceStatus)
	mux.HandleFunc("GET /sandboxes/{id}/service/logs", s.handleServiceLogs)
	// Durable per-sandbox logs (service output + exec activity). 501 when
	// Config.LogStore is nil.
	mux.HandleFunc("GET /sandboxes/{id}/logs", s.handleSandboxLogs)
	mux.HandleFunc("GET /sandboxes/{id}/capture", s.handleCapture)
	// daemon backup: streams a tar.gz of the daemon's stores. Gated
	// by the default-deny `admin_backup` op (carries registry secrets).
	mux.HandleFunc("GET /admin/backup", s.handleAdminBackup)
	mux.HandleFunc("POST /sandboxes/{id}/snapshot", s.handleCreateSnapshot)
	// Sandbox-level in-place sleep/wake: the low-level primitive
	// behind scale-to-zero, parallel to snapshot/fork.
	mux.HandleFunc("POST /sandboxes/{id}/sleep", s.handleSleepSandbox)
	mux.HandleFunc("POST /sandboxes/{id}/wake", s.handleWakeSandbox)
	mux.HandleFunc("GET /snapshots", s.handleListSnapshots)
	mux.HandleFunc("GET /snapshots/{id}", s.handleGetSnapshot)
	mux.HandleFunc("DELETE /snapshots/{id}", s.handleDeleteSnapshot)
	mux.HandleFunc("POST /snapshots/{id}/fork", s.handleForkSnapshot)
	mux.HandleFunc("GET /profiles", s.handleListProfiles)
	// Durable apps (v0.4): a named workload reconciled into a running
	// instance that survives daemon restart. 501 when AppManager is nil.
	mux.HandleFunc("POST /apps", s.handleCreateApp)
	mux.HandleFunc("GET /apps", s.handleListApps)
	mux.HandleFunc("GET /apps/{name}", s.handleGetApp)
	mux.HandleFunc("PUT /apps/{name}", s.handleUpdateApp)
	mux.HandleFunc("DELETE /apps/{name}", s.handleDeleteApp)
	// Operate a deployed app by name: resolve name → current instance per
	// request (redeploy-safe), then delegate to the sandbox exec/logs handlers.
	mux.HandleFunc("POST /apps/{name}/exec", s.handleAppExec)
	mux.HandleFunc("GET /apps/{name}/exec", s.handleAppExecWS)
	mux.HandleFunc("GET /apps/{name}/logs", s.handleAppLogs)
	// Scale-to-zero (v0.5.0): sleep frees the instance's RAM, wake restores it.
	mux.HandleFunc("POST /apps/{name}/sleep", s.handleSleepApp)
	mux.HandleFunc("POST /apps/{name}/wake", s.handleWakeApp)
	// Custom domains (v0.7.0): attach/detach/list FQDNs the ingress proxy routes
	// (and, in terminate mode, obtains a cert for). Config ops, gated as exec.
	mux.HandleFunc("GET /apps/{name}/domains", s.handleListAppDomains)
	mux.HandleFunc("POST /apps/{name}/domains", s.handleAddAppDomain)
	mux.HandleFunc("DELETE /apps/{name}/domains/{domain}", s.handleRemoveAppDomain)
	// OCI image cache (experimental). When Config.Images is nil these
	// answer 501. Mutations gate as `create`, reads as `read`.
	// Private-registry credentials (v0.4.4): manage the creds used for image
	// pulls. The secret is write-only; GET/DELETE never return it.
	mux.HandleFunc("POST /registry/credentials", s.handleRegistryLogin)
	mux.HandleFunc("GET /registry/credentials", s.handleRegistryList)
	mux.HandleFunc("DELETE /registry/credentials/{host}", s.handleRegistryLogout)

	mux.HandleFunc("POST /images", s.handlePullImage)
	mux.HandleFunc("POST /images/import", s.handleImportImage)
	mux.HandleFunc("GET /images", s.handleListImages)
	mux.HandleFunc("GET /images/{ref}", s.handleGetImage)
	mux.HandleFunc("DELETE /images/{ref}", s.handleDeleteImage)

	// Persistent volumes (v0.6.0). 501 when Config.Volumes is nil. Mutations
	// gate as `create`/`delete`, reads as `read`.
	mux.HandleFunc("POST /volumes", s.handleCreateVolume)
	mux.HandleFunc("GET /volumes", s.handleListVolumes)
	mux.HandleFunc("GET /volumes/{name}", s.handleGetVolume)
	mux.HandleFunc("DELETE /volumes/{name}", s.handleDeleteVolume)
	// Volume backups (v0.6.3). Create gates as `snapshot`, list as `read`,
	// delete as `delete`.
	mux.HandleFunc("POST /volumes/{name}/backups", s.handleBackupVolume)
	mux.HandleFunc("GET /volumes/{name}/backups", s.handleListBackups)
	mux.HandleFunc("GET /backups", s.handleListBackups)
	mux.HandleFunc("DELETE /backups/{id}", s.handleDeleteBackup)
	// Off-host export/import: stream a backup's bytes off the host (the CP ships
	// them to a provider) and back onto a host for restore. Both gate as
	// `volume_backup` (default-deny, moves volume data).
	mux.HandleFunc("GET /backups/{id}/export", s.handleExportBackup)
	mux.HandleFunc("POST /backups/import", s.handleImportBackup)
	// Restore/clone create a new volume — gate as `create`.
	mux.HandleFunc("POST /volumes/{name}/restore", s.handleRestoreVolume)
	mux.HandleFunc("POST /volumes/{name}/clone", s.handleCloneVolume)
	return mux
}

// --- request validation & response mapping ---------------------------
//
// The wire types themselves live in sdk/api (shared with the
// client). Validation stays here because it pulls in server-only deps
// (internal/network); the response mappers stay here because they read
// the manager's internal sandbox/snapshot structs.

// netParams is the parsed, validated egress config for one sandbox: the
// hostname allowlist plus the two range-based modes (full-egress, CIDRs).
type netParams struct {
	allowlist  *network.Allowlist
	fullEgress bool
	cidrs      []netip.Prefix
}

// validateNetwork parses and validates a NetworkRequest. Rules: enabled=false
// with any egress option set is a 400 (inconsistent); enabled=true requires at
// least one of allowlist / full-egress / CIDR; an invalid hostname pattern or
// CIDR is a 400. Returns (nil, 0, nil) for "no network". The public-hosts-only
// invariant is enforced downstream at the nft/DNS layer, not here.
func validateNetwork(r *api.NetworkRequest) (*netParams, int, error) {
	anyEgress := len(r.Allowlist) > 0 || r.FullEgress || len(r.AllowlistCIDR) > 0
	if !r.Enabled {
		if anyEgress {
			return nil, http.StatusBadRequest,
				errors.New("network egress options set but network.enabled is false")
		}
		return nil, 0, nil
	}
	if !anyEgress {
		return nil, http.StatusBadRequest,
			errors.New("network.enabled=true requires an allowlist, full_egress, or allowlist_cidr")
	}
	// The hostname allowlist may be empty when full-egress or CIDRs carry the
	// egress (network.New([]) yields a matcher that matches nothing).
	al, err := network.New(r.Allowlist)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("network.allowlist: %w", err)
	}
	var cidrs []netip.Prefix
	for _, c := range r.AllowlistCIDR {
		p, perr := netip.ParsePrefix(c)
		if perr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("allowlist_cidr %q: %w", c, perr)
		}
		if !p.Addr().Is4() {
			return nil, http.StatusBadRequest, fmt.Errorf("allowlist_cidr %q: only IPv4 prefixes are supported", c)
		}
		cidrs = append(cidrs, p.Masked())
	}
	return &netParams{allowlist: al, fullEgress: r.FullEgress, cidrs: cidrs}, 0, nil
}

// validatePublish checks the port mappings and returns the sandbox-layer
// form. Rules: ports in 1..65535; protocol tcp only (udp/others rejected
// for now); no duplicate host bind (same host IP + host port) within one
// request.
func validatePublish(mappings []api.PortMapping) ([]sandbox.PortMapping, error) {
	out := make([]sandbox.PortMapping, 0, len(mappings))
	seen := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		proto := m.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			return nil, fmt.Errorf("publish: protocol %q not supported (tcp only)", proto)
		}
		if m.HostPort < 1 || m.HostPort > 65535 {
			return nil, fmt.Errorf("publish: host_port %d out of range (1..65535)", m.HostPort)
		}
		if m.GuestPort < 1 || m.GuestPort > 65535 {
			return nil, fmt.Errorf("publish: guest_port %d out of range (1..65535)", m.GuestPort)
		}
		key := net.JoinHostPort(m.HostIP, strconv.Itoa(m.HostPort))
		if seen[key] {
			return nil, fmt.Errorf("publish: host port %s mapped more than once", key)
		}
		seen[key] = true
		out = append(out, sandbox.PortMapping{
			HostIP:    m.HostIP,
			HostPort:  m.HostPort,
			GuestPort: m.GuestPort,
			Protocol:  proto,
		})
	}
	return out, nil
}

// exposedPortPublish expands an image's declared EXPOSE ports into publish
// mappings for `-P` (docker's publish-all, but deterministic: guest port N →
// host port N). Only tcp ports are published — crucible publish is tcp-only, so
// a udp EXPOSE is skipped. A guest port already covered by an explicit -p is
// left to that mapping (explicit wins). rc == nil (a profile, not an image)
// yields nothing.
func exposedPortPublish(rc *oci.RunConfig, explicit []api.PortMapping) ([]api.PortMapping, error) {
	if rc == nil || len(rc.ExposedPorts) == 0 {
		return nil, nil
	}
	haveGuest := make(map[int]bool, len(explicit))
	for _, m := range explicit {
		haveGuest[m.GuestPort] = true
	}
	var out []api.PortMapping
	for _, e := range rc.ExposedPorts {
		port, proto, err := parseExposedPort(e)
		if err != nil {
			return nil, err
		}
		if proto != "tcp" || haveGuest[port] {
			continue
		}
		out = append(out, api.PortMapping{HostPort: port, GuestPort: port, Protocol: "tcp"})
	}
	return out, nil
}

// parseExposedPort parses an OCI ExposedPorts entry ("8080/tcp", or a bare
// "8080" which OCI treats as tcp) into a port number and protocol.
func parseExposedPort(s string) (int, string, error) {
	numStr, proto := s, "tcp"
	if i := strings.IndexByte(s, '/'); i >= 0 {
		numStr, proto = s[:i], strings.ToLower(s[i+1:])
	}
	port, err := strconv.Atoi(numStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("exposed port %q: bad port number", s)
	}
	return port, proto, nil
}

// ociInitBootArgs is the kernel command line for a converted OCI image:
// the runtime defaults plus init=<injected agent>, so the guest boots
// crucible-agent as PID 1 instead of the (absent) /sbin/init.
const ociInitBootArgs = runner.DefaultBootArgs + " init=/" + oci.InjectedAgentPath

// imageError pairs an HTTP status with an error for image resolution.
type imageErr struct {
	status int
	err    error
}

// Pull policy values for CreateSandboxRequest.Pull, mirroring
// `docker run --pull`. Empty is treated as pullMissing.
const (
	pullMissing = "missing"
	pullAlways  = "always"
	pullNever   = "never"
)

// validatePull normalizes and checks a pull policy at the request
// boundary. Empty defaults to "missing" (acquire on a store miss).
func validatePull(p string) (string, error) {
	switch p {
	case "", pullMissing:
		return pullMissing, nil
	case pullAlways, pullNever:
		return p, nil
	default:
		return "", fmt.Errorf("invalid pull policy %q (want missing|always|never)", p)
	}
}

// acquireImage resolves an OCI ref to a store record under the given
// pull policy (docker-run semantics): "missing" acquires on a store
// miss, "always" re-pulls unconditionally, "never" never touches the
// network. Get's not-found/ambiguous errors and any pull/convert
// failure propagate for resolveImage to map to a status code.
func (s *Server) acquireImage(ctx context.Context, ref, pull string, auth *oci.PullAuth) (*oci.ImageRecord, error) {
	switch pull {
	case pullAlways:
		return s.cfg.Images.Pull(ctx, ref, auth)
	case pullNever:
		return s.cfg.Images.Get(ref)
	default: // pullMissing (and the empty value, normalized by validatePull)
		rec, err := s.cfg.Images.Get(ref)
		if errors.Is(err, oci.ErrImageNotFound) {
			return s.cfg.Images.Pull(ctx, ref, auth)
		}
		return rec, err
	}
}

// buildCreateConfig resolves a CreateSandboxRequest into a
// sandbox.CreateConfig: image resolution (→ rootfs + boot args + effective
// service), network validation, port publish, and service validation. It
// mutates req.BootArgs and req.Service with image-derived values so a
// caller can apply policy against the effective request. TokenID is left
// unset for the caller to fill. Shared by handleCreateSandbox and the app
// instantiator so an app boots through the exact same path a `create` does.
func (s *Server) buildCreateConfig(ctx context.Context, req *api.CreateSandboxRequest, pull string) (sandbox.CreateConfig, *imageErr) {
	imgRec, imgBootArgs, ierr := s.resolveImage(ctx, req.Image, pull, toPullAuth(req.RegistryAuth))
	if ierr != nil {
		return sandbox.CreateConfig{}, ierr
	}
	var rootfsOverride string
	staticNetwork := false
	if imgRec != nil {
		rootfsOverride = imgRec.RootfsPath
		req.BootArgs = imgBootArgs
		// OCI guests have no DHCP client; the network is pushed over vsock.
		staticNetwork = true
		// Run the image's own entrypoint merged with any override
		// (docker-run semantics); nil for a bare sandbox.
		req.Service = effectiveServiceSpec(imgRec.RunConfig, req.Service)
	}

	var netCfg *sandbox.NetworkConfig
	if req.Network != nil {
		np, status, err := validateNetwork(req.Network)
		if err != nil {
			return sandbox.CreateConfig{}, &imageErr{status, err}
		}
		if np != nil {
			netCfg = &sandbox.NetworkConfig{Allowlist: np.allowlist, FullEgress: np.fullEgress, CIDRs: np.cidrs}
		}
	}

	// -P / publish-all: expand the image's declared EXPOSE ports into
	// publish mappings (guest N → host N). Only an OCI image carries EXPOSE
	// metadata; an explicit -p for a guest port wins over the auto-mapping.
	if req.PublishAll && imgRec != nil {
		exposed, err := exposedPortPublish(imgRec.RunConfig, req.Publish)
		if err != nil {
			return sandbox.CreateConfig{}, &imageErr{http.StatusBadRequest, err}
		}
		req.Publish = append(req.Publish, exposed...)
	}

	// Port publish: ensure a NIC to forward to; synthesize an
	// egress-denied one when none was requested (reachable, can't phone home).
	var publish []sandbox.PortMapping
	if len(req.Publish) > 0 {
		pm, err := validatePublish(req.Publish)
		if err != nil {
			return sandbox.CreateConfig{}, &imageErr{http.StatusBadRequest, err}
		}
		publish = pm
		if netCfg == nil {
			denyAll, nerr := network.New(nil)
			if nerr != nil {
				return sandbox.CreateConfig{}, &imageErr{http.StatusInternalServerError, nerr}
			}
			netCfg = &sandbox.NetworkConfig{Allowlist: denyAll}
		}
	}

	// Sandbox-level env is merged into the entrypoint's environment (req.Env
	// wins over the image ENV), exactly like an app's env. A bare profile
	// sandbox has no entrypoint service, so its env applies only to explicit
	// exec (ExecRequest.Env), not here.
	if len(req.Env) > 0 && req.Service != nil {
		mergeAppEnv(req.Service, req.Env)
	}

	if req.Service != nil {
		if err := validateServiceSpec(req.Service); err != nil {
			return sandbox.CreateConfig{}, &imageErr{http.StatusBadRequest, err}
		}
	}

	volumes := make([]sandbox.VolumeMount, 0, len(req.Volumes))
	for _, v := range req.Volumes {
		if v.Name == "" || !strings.HasPrefix(v.Path, "/") {
			return sandbox.CreateConfig{}, &imageErr{http.StatusBadRequest,
				fmt.Errorf("volume requires a name and an absolute path (got name=%q path=%q)", v.Name, v.Path)}
		}
		volumes = append(volumes, sandbox.VolumeMount{Name: v.Name, Path: v.Path})
	}

	return sandbox.CreateConfig{
		VCPUs:          req.VCPUs,
		MemoryMiB:      req.MemoryMiB,
		BootArgs:       req.BootArgs,
		TimeoutSec:     req.TimeoutSec,
		Profile:        req.Profile,
		RootfsOverride: rootfsOverride,
		StaticNetwork:  staticNetwork,
		Network:        netCfg,
		Service:        req.Service,
		Publish:        publish,
		DiskBytes:      req.DiskBytes,
		Volumes:        volumes,
	}, nil
}

// resolveImage turns a request's ImageRef into a store record + boot
// args, acquiring the image on a store miss per the pull policy so that
// `create --image nginx:alpine` Just Works like `docker run`. No image
// → (nil, "", nil). A path override is still unimplemented. Exactly one
// of Path/OCI must be set.
func (s *Server) resolveImage(ctx context.Context, ref *api.ImageRef, pull string, auth *oci.PullAuth) (rec *oci.ImageRecord, bootArgs string, ierr *imageErr) {
	if ref == nil {
		return nil, "", nil
	}
	pathSet := ref.Path != ""
	ociSet := ref.OCI != ""
	switch {
	case pathSet && ociSet:
		return nil, "", &imageErr{http.StatusBadRequest, errors.New("image.path and image.oci are mutually exclusive")}
	case !pathSet && !ociSet:
		return nil, "", &imageErr{http.StatusBadRequest, errors.New("image must set either path or oci")}
	case pathSet:
		return nil, "", &imageErr{http.StatusNotImplemented, errors.New("image.path per-sandbox override not implemented (use image.oci or a profile)")}
	}
	// OCI reference.
	if s.cfg.Images == nil {
		return nil, "", &imageErr{http.StatusNotImplemented, errors.New("image support is not enabled on this daemon (set --image-dir)")}
	}
	rec, err := s.acquireImage(ctx, ref.OCI, pull, auth)
	if err != nil {
		switch {
		case errors.Is(err, oci.ErrImageNotFound):
			// Only reachable under pull=never (missing/always would have
			// pulled). Point the operator at the one-command fix.
			return nil, "", &imageErr{http.StatusNotFound,
				fmt.Errorf("image %q not in the store and --pull=never; pull it first or use --pull missing", ref.OCI)}
		case errors.Is(err, oci.ErrAmbiguousImage):
			return nil, "", &imageErr{http.StatusConflict, err}
		default:
			// Pull/convert failure (unknown ref, registry unreachable,
			// mkfs error) — gateway class, mirroring imageError.
			return nil, "", &imageErr{http.StatusBadGateway, err}
		}
	}
	if rec.RootfsPath == "" {
		return nil, "", &imageErr{http.StatusInternalServerError, errors.New("image record has no rootfs path")}
	}
	return rec, ociInitBootArgs, nil
}

// effectiveServiceSpec computes the service the sandbox runs from a
// converted image's runtime contract, merged with an optional request
// override using docker-run semantics:
//   - command: image Entrypoint+Cmd by default; a non-empty override
//     Cmd replaces it entirely (like a trailing command / --entrypoint).
//   - env: image Env overlaid by override Env; the result is the exact
//     process environment (EnvExact), no agent-environ leakage.
//   - user / cwd / stop-signal: image value, override wins when set.
//   - restart / stop-grace / log-buffer: from the override (not an
//     image concept).
//
// Returns nil when there is nothing to run (no entrypoint/cmd and no
// override) — a bare sandbox to exec into.
func effectiveServiceSpec(rc *oci.RunConfig, override *wire.ServiceSpec) *wire.ServiceSpec {
	spec := &wire.ServiceSpec{EnvExact: true}
	envMap := map[string]string{}
	if rc != nil {
		spec.Cmd = append(append([]string{}, rc.Entrypoint...), rc.Cmd...)
		spec.Cwd = rc.WorkingDir
		spec.User = rc.User
		spec.StopSignal = normalizeStopSignal(rc.StopSignal)
		for _, e := range rc.Env {
			k, v, _ := strings.Cut(e, "=")
			envMap[k] = v
		}
	}
	if override != nil {
		if len(override.Cmd) > 0 {
			spec.Cmd = override.Cmd
		}
		if override.Cwd != "" {
			spec.Cwd = override.Cwd
		}
		if override.User != "" {
			spec.User = override.User
		}
		if override.StopSignal != "" {
			spec.StopSignal = override.StopSignal
		}
		for k, v := range override.Env {
			envMap[k] = v
		}
		spec.Restart = override.Restart
		spec.StopGraceSec = override.StopGraceSec
		spec.LogBufferBytes = override.LogBufferBytes
	}
	if len(spec.Cmd) == 0 {
		return nil
	}
	if len(envMap) > 0 {
		spec.Env = envMap
	}
	return spec
}

// normalizeStopSignal turns a numeric OCI StopSignal ("9") into its
// name ("SIGKILL") so the wire always carries a name the agent and
// validation understand; names pass through unchanged.
func normalizeStopSignal(s string) string {
	if s == "" {
		return ""
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return s
	}
	if name := unix.SignalName(syscall.Signal(n)); name != "" {
		return name
	}
	return s
}

func sandboxResponseFrom(sb *sandbox.Sandbox) api.SandboxResponse {
	resp := api.SandboxResponse{
		ID:               sb.ID,
		VCPUs:            sb.VCPUs,
		MemoryMiB:        sb.MemoryMiB,
		Workdir:          sb.Workdir,
		Profile:          sb.Profile,
		CreatedAt:        sb.CreatedAt,
		SourceSnapshotID: sb.SourceSnapshotID,
	}
	if sb.Network != nil {
		resp.Network = &api.NetworkResponse{
			Enabled: true,
			GuestIP: sb.Network.GuestIP,
			Gateway: sb.Network.Gateway,
		}
		if sb.Network.Allowlist != nil {
			resp.Network.Allowlist = sb.Network.Allowlist.Patterns()
		}
	}
	for _, p := range sb.Published {
		resp.Published = append(resp.Published, api.PortMapping{
			HostIP:    p.HostIP,
			HostPort:  p.HostPort,
			GuestPort: p.GuestPort,
			Protocol:  p.Protocol,
		})
	}
	return resp
}

func snapshotResponseFrom(s *sandbox.Snapshot) api.SnapshotResponse {
	return api.SnapshotResponse{
		ID:         s.ID,
		SourceID:   s.SourceID,
		VCPUs:      s.VCPUs,
		MemoryMiB:  s.MemoryMiB,
		Dir:        s.Dir,
		StatePath:  s.StatePath,
		MemPath:    s.MemPath,
		RootfsPath: s.RootfsPath,
		CreatedAt:  s.CreatedAt,
	}
}

// --- handlers --------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleListProfiles returns the rootfs profiles the daemon was
// configured with via --rootfs-dir (empty when none). Lets the CLI show
// `crucible profile ls` without the operator inspecting the daemon flags.
func (s *Server) handleListProfiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.ProfilesResponse{Profiles: s.cfg.Manager.Profiles()})
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	// Cap request body so a malicious client can't exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req api.CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Empty body is acceptable — all fields are optional.
		if !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
			return
		}
	}

	pull, err := validatePull(req.Pull)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Image resolution can pull + convert a multi-hundred-MB image
	// (minutes) when the ref isn't cached, so clear the one-shot write
	// deadline the same way the pull/snapshot handlers do; r.Context()
	// still bounds the work.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Resolve the request into a CreateConfig (image → rootfs, network,
	// publish, service). Shared with the app instantiator (see
	// buildCreateConfig); it mutates req.BootArgs/Service with
	// image-derived values so policy below sees the effective request.
	cfg, ierr := s.buildCreateConfig(r.Context(), &req, pull)
	if ierr != nil {
		writeError(w, ierr.status, ierr.err)
		return
	}

	// Scoped-token ceilings — every violation reported at once. max_sandboxes is
	// counted per token (CountByToken), so one token can't consume another's
	// budget (best-effort: the count races a concurrent create).
	tokenID := tokenIDFor(r)
	if pol := policyFor(r); pol != nil {
		var reqNet []string
		wantFull, wantCIDR := false, false
		if req.Network != nil && req.Network.Enabled {
			reqNet = req.Network.Allowlist
			wantFull = req.Network.FullEgress
			wantCIDR = len(req.Network.AllowlistCIDR) > 0
		}
		if err := errors.Join(
			pol.CheckProfile(req.Profile),
			pol.CheckNetAllow(reqNet),
			pol.CheckFullEgress(wantFull, wantCIDR),
			pol.CheckVCPUs(req.VCPUs),
			pol.CheckMemory(req.MemoryMiB),
			pol.CheckCapacity(s.cfg.Manager.CountByToken(tokenID), 1),
		); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		// Bundling a service into create configures what runs in the
		// guest — exec-grade power. A create-only token must not smuggle
		// an entrypoint past the operation gate.
		if req.Service != nil && !pol.Allows(policy.OpExec) {
			writeError(w, http.StatusForbidden,
				errors.New("this token is not permitted to exec (required for create with a service)"))
			return
		}
	}

	cfg.TokenID = tokenID
	sb, err := s.cfg.Manager.Create(r.Context(), cfg)
	if err != nil {
		if errors.Is(err, sandbox.ErrInvalidConfig) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// A service-bearing sandbox gets a background drain of its output into
	// the durable log store (so `crucible logs` works, and survives the
	// sandbox). Self-terminates when the sandbox is deleted.
	if req.Service != nil && s.cfg.LogStore != nil {
		s.startServiceDrain(sb.ID)
	}
	writeJSON(w, http.StatusCreated, sandboxResponseFrom(sb))
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, _ *http.Request) {
	all := s.cfg.Manager.List()
	out := make([]api.SandboxResponse, 0, len(all))
	for _, sb := range all {
		out = append(out, sandboxResponseFrom(sb))
	}
	writeJSON(w, http.StatusOK, api.ListResponse{Sandboxes: out})
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	sb, err := s.cfg.Manager.Get(id)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxResponseFrom(sb))
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	if err := s.cfg.Manager.Delete(r.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleExecSandbox runs a command inside the sandbox via the guest
// agent and streams its output back to the HTTP client. The response
// body is a sequence of wire frames — identical in shape to what
// the agent itself produces, so clients can parse it the same way.
//
// Error handling has two phases:
//   - Before the 200 is sent: validation errors come back as plain 4xx
//     JSON ({"error": "..."}).
//   - After the 200 is sent: we've committed to a streamed body, so
//     any error (failed to reach the agent, connection died) is
//     reported as a synthesized FrameExit with ExitCode=-1 and Error
//     populated. This keeps the framing contract intact.
func (s *Server) handleExecSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req wire.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("cmd is required"))
		return
	}

	// Scoped-token timeout ceiling: clamp (don't reject) the command deadline.
	if pol := policyFor(r); pol != nil {
		req.TimeoutSec = pol.ClampTimeout(req.TimeoutSec)
	}

	// Validate the sandbox exists before committing to a streamed
	// response — gives us a clean 404 for the common mistake.
	if _, err := s.cfg.Manager.Get(id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Interactive exec is a separate, hijacked, full-duplex path. The
	// one-shot streaming path below is untouched.
	if r.URL.Query().Get("stdin") == "1" {
		s.handleExecInteractive(w, r, id, req)
		return
	}

	// Exec streams for as long as the command runs — untrusted builds and
	// tests routinely exceed the server's WriteTimeout. That timeout is
	// armed once at request start and never reset per write, so leaving it
	// in place truncates any exec > WriteTimeout mid-stream. Clear the write
	// deadline for this response; r.Context() still bounds the exec.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Commit to streaming.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	// Flush per frame through the ResponseController so it walks the
	// middleware's Unwrap chain to the real Flusher — a direct w.(http.Flusher)
	// assertion fails against loggingResponseWriter (its embedded-interface
	// method set doesn't promote Flush), leaving frames buffered until the
	// command exits. rc is the controller created above.
	fw := wire.NewFrameWriter(flushOnWrite{w: w, rc: rc})

	stdoutStream, stderrStream := fw.Stream(wire.FrameStdout), fw.Stream(wire.FrameStderr)

	// Durable exec-activity capture: bracket the run with start/exit
	// events and tee its output into the log store, so `crucible logs
	// --source exec` shows what ran and what it produced.
	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: "exec: " + strings.Join(req.Cmd, " "),
		})
		stdoutStream = io.MultiWriter(stdoutStream, execLogWriter{s: s, id: id, stream: logstore.StreamStdout})
		stderrStream = io.MultiWriter(stderrStream, execLogWriter{s: s, id: id, stream: logstore.StreamStderr})
	}

	result, err := s.cfg.Manager.Exec(r.Context(), id, req, stdoutStream, stderrStream)
	if err != nil {
		// Exec plumbing broke (agent unreachable, connection died,
		// etc.). Synthesize an exit frame so the client still sees a
		// well-formed stream.
		result = wire.ExecResult{ExitCode: -1, Error: err.Error()}
	}
	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: fmt.Sprintf("exit %d", result.ExitCode),
		})
	}

	payload, jerr := json.Marshal(result)
	if jerr != nil {
		payload = []byte(fmt.Sprintf(`{"exit_code":-1,"error":%q}`, jerr.Error()))
	}
	_ = fw.WriteFrame(wire.FrameExit, payload)
}

// maxFilesBody caps a PUT-files (`crucible cp` push) tar body. The daemon is a
// streaming proxy, so this bounds a runaway upload without buffering; the agent
// enforces the same cap on its side.
const maxFilesBody = 1 << 30

// handlePutFiles streams a tar body (the `crucible cp` push path) into a
// sandbox's guest filesystem beneath ?path=<dest>. It is a pure streaming proxy
// to the guest agent: r.Body flows straight through Manager.PutFiles, nothing
// is buffered whole. Gated as exec-grade (see operationFor).
func (s *Server) handlePutFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	dest := r.URL.Query().Get("path")
	if dest == "" {
		writeError(w, http.StatusBadRequest, errors.New("path query param (guest destination dir) is required"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFilesBody)

	res, err := s.cfg.Manager.PutFiles(r.Context(), id, dest, r.Body)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: fmt.Sprintf("cp: wrote %d file(s), %d bytes to %s", res.Files, res.Bytes, dest),
		})
	}
	writeJSON(w, http.StatusOK, res)
}

// defaultFileReadBytes caps a GET-files single-file read (the read_file MCP
// tool) when ?max_bytes= is unset.
const defaultFileReadBytes = 10 << 20

// handleGetFile returns the bytes of a single guest file (GET
// /sandboxes/{id}/files?path=<f>&max_bytes=<n>) — a content read, guest -> host.
// Only file bytes flow out and nothing is written host-side, so there is no
// traversal surface. Gated as read (GET; see operationFor).
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, errors.New("path query param (guest file) is required"))
		return
	}
	maxBytes := defaultFileReadBytes
	if v := r.URL.Query().Get("max_bytes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxBytes = n
		}
	}

	data, err := s.cfg.Manager.ReadFile(r.Context(), id, p, maxBytes)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleExecInteractive bridges a hijacked client connection to a hijacked
// guest-agent connection for a full-duplex interactive exec (a live shell).
// The daemon becomes a bidirectional frame relay: raw stdin frames flow
// client→agent, and agent→client frames are parsed so exec output can be
// teed into the durable log store before being forwarded verbatim.
//
// The agent conn is established first so a dial failure is a clean HTTP
// error before the client conn is hijacked and the framing contract begins.
func (s *Server) handleExecInteractive(w http.ResponseWriter, r *http.Request, id string, req wire.ExecRequest) {
	agentConn, err := s.cfg.Manager.ExecInteractive(r.Context(), id, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("interactive exec: %w", err))
		return
	}
	defer func() { _ = agentConn.Close() }()

	rc := http.NewResponseController(w)
	clientConn, clientBuf, err := rc.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("hijack: %w", err))
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n"); err != nil {
		return
	}

	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: "exec (interactive): " + strings.Join(req.Cmd, " "),
		})
	}

	// Client → agent: forward raw stdin frame bytes. When the client goes
	// away (EOF/error), close the agent conn so the guest kills the command.
	go func() {
		_, _ = io.Copy(agentConn, clientBuf.Reader)
		_ = agentConn.Close()
	}()

	// Agent → client: parse frames (to tee output), forward each verbatim.
	exit := s.relayExecFrames(id, agentConn, clientConn)

	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: fmt.Sprintf("exit %d", exit),
		})
	}
}

// relayExecFrames copies wire frames from the guest agent to the
// client, teeing stdout/stderr payloads into the durable log store. It
// returns the exit code carried by the terminal FrameExit (-1 if the stream
// ends without one). Writes to the client are best-effort: a dead client
// just ends the relay.
func (s *Server) relayExecFrames(id string, agentConn io.Reader, clientConn io.Writer) int {
	exit := -1
	for {
		f, err := wire.ReadFrame(agentConn)
		if err != nil {
			return exit
		}
		switch f.Type {
		case wire.FrameStdout:
			if s.cfg.LogStore != nil {
				s.appendLog(id, logstore.Record{
					TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamStdout, Text: string(f.Payload),
				})
			}
		case wire.FrameStderr:
			if s.cfg.LogStore != nil {
				s.appendLog(id, logstore.Record{
					TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamStderr, Text: string(f.Payload),
				})
			}
		case wire.FrameExit:
			var res wire.ExecResult
			if json.Unmarshal(f.Payload, &res) == nil {
				exit = res.ExitCode
			}
		}
		if err := wire.WriteFrame(clientConn, f.Type, f.Payload); err != nil {
			return exit
		}
		if f.Type == wire.FrameExit {
			return exit
		}
	}
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}

	// A large-guest snapshot writes the whole guest memory file and can run
	// well past the server's WriteTimeout, which is armed once at request
	// start and never reset per write. Left in place it truncates the
	// response: the snapshot is written, registered, and journaled
	// server-side, but writeJSON fails on the long-passed deadline and the
	// client never learns the snapshot ID (orphan). Clear the write deadline
	// as /exec does; r.Context() still bounds the request.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	snap, err := s.cfg.Manager.Snapshot(r.Context(), id)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshotResponseFrom(snap))
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, _ *http.Request) {
	all := s.cfg.Manager.ListSnapshots()
	out := make([]api.SnapshotResponse, 0, len(all))
	for _, snap := range all {
		out = append(out, snapshotResponseFrom(snap))
	}
	writeJSON(w, http.StatusOK, api.SnapshotListResponse{Snapshots: out})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidSnapshotID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid snapshot id"))
		return
	}
	snap, err := s.cfg.Manager.GetSnapshot(id)
	if err != nil {
		if errors.Is(err, sandbox.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotResponseFrom(snap))
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidSnapshotID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid snapshot id"))
		return
	}
	if err := s.cfg.Manager.DeleteSnapshot(r.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleForkSnapshot creates `count` sandboxes from the given snapshot.
// `count` comes from the query string (?count=N), defaults to 1 if
// absent. Fork is all-or-nothing: a failure part-way through rolls
// back any forks started so far, so the response is either "all N
// sandboxes" or an error.
func (s *Server) handleForkSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidSnapshotID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid snapshot id"))
		return
	}

	count := 1
	if q := r.URL.Query().Get("count"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count %q (want a positive integer)", q))
			return
		}
		count = n
	}

	// Optional JSON body (api.ForkRequest): body count wins over the query
	// param; publish maps host ports onto the fork. An empty body keeps the
	// legacy query-only form working.
	var publish []sandbox.PortMapping
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var freq api.ForkRequest
	if err := json.NewDecoder(r.Body).Decode(&freq); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
		return
	}
	if freq.Count != 0 {
		if freq.Count < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count %d (want a positive integer)", freq.Count))
			return
		}
		count = freq.Count
	}
	if len(freq.Publish) > 0 {
		if count != 1 {
			writeError(w, http.StatusBadRequest,
				errors.New("publish requires count 1: host ports are exclusive, a fan-out cannot share them"))
			return
		}
		pm, err := validatePublish(freq.Publish)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		publish = pm
	}
	// Reject an oversized fan-out here so we never even hand a huge count to
	// Fork (which allocates proportional to it). Fork enforces the same
	// bound as defense-in-depth.
	if max := s.cfg.Manager.MaxForkCount(); count > max {
		writeError(w, http.StatusBadRequest, fmt.Errorf("count %d exceeds max %d", count, max))
		return
	}

	// Scoped-token ceilings: the fork count and the token's resulting sandbox total.
	tokenID := tokenIDFor(r)
	if pol := policyFor(r); pol != nil {
		if err := errors.Join(
			pol.CheckFork(count),
			pol.CheckCapacity(s.cfg.Manager.CountByToken(tokenID), count),
		); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
	}

	// Forking N large guests can outlast the server's WriteTimeout (armed
	// once at request start, never reset per write). Left in place the forks
	// boot, refresh identity, and register server-side, but writeJSON fails
	// on the long-passed deadline: the client believes the fork failed while
	// the sandboxes are live and unreaped (leak). Clear the write deadline as
	// /exec does; r.Context() still bounds the request.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	forks, err := s.cfg.Manager.Fork(r.Context(), id, count, tokenID, publish)
	if err != nil {
		if errors.Is(err, sandbox.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, sandbox.ErrInvalidConfig) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.SandboxResponse, 0, len(forks))
	for _, f := range forks {
		out = append(out, sandboxResponseFrom(f))
	}
	writeJSON(w, http.StatusCreated, api.ForkResponse{Sandboxes: out})
}

// flushOnWrite forwards every Write to w and flushes the underlying
// chunked response so frames appear on the wire byte-for-byte as the
// agent produces them. Without the Flush the stdlib would buffer and
// the client would only see output when the command exits.
type flushOnWrite struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushOnWrite) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		// ErrNotSupported only on a writer with no Flusher anywhere in its
		// Unwrap chain (e.g. HTTP/2 already auto-flushes); ignore it.
		_ = f.rc.Flush()
	}
	return n, err
}

// --- small helpers ----------------------------------------------------

// writeJSON serializes v as JSON and writes it with the given status.
// On encoding failures we can't change the status — headers are already
// sent — so we log and move on.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, api.ErrorResponse{Error: err.Error(), Code: errCode(err)})
}
