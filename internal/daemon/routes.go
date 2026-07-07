package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/sandbox"
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
	mux.HandleFunc("POST /sandboxes/{id}/snapshot", s.handleCreateSnapshot)
	mux.HandleFunc("GET /snapshots", s.handleListSnapshots)
	mux.HandleFunc("GET /snapshots/{id}", s.handleGetSnapshot)
	mux.HandleFunc("DELETE /snapshots/{id}", s.handleDeleteSnapshot)
	mux.HandleFunc("POST /snapshots/{id}/fork", s.handleForkSnapshot)
	mux.HandleFunc("GET /profiles", s.handleListProfiles)
	return mux
}

// --- request validation & response mapping ---------------------------
//
// The wire types themselves live in internal/api (shared with the
// client). Validation stays here because it pulls in server-only deps
// (internal/network); the response mappers stay here because they read
// the manager's internal sandbox/snapshot structs.

// validateNetwork enforces v0.1 network semantics and returns the parsed
// allowlist on success. Rules: enabled=false with a populated allowlist
// is a 400 (inconsistent); enabled=true requires a non-empty allowlist
// (no full-internet egress in v0.1); an invalid pattern is a 400.
func validateNetwork(r *api.NetworkRequest) (*network.Allowlist, int, error) {
	if !r.Enabled {
		if len(r.Allowlist) > 0 {
			return nil, http.StatusBadRequest,
				errors.New("network.allowlist set but network.enabled is false")
		}
		return nil, 0, nil
	}
	if len(r.Allowlist) == 0 {
		return nil, http.StatusBadRequest,
			errors.New("network.enabled=true requires a non-empty allowlist (full-internet egress is not supported in v0.1)")
	}
	al, err := network.New(r.Allowlist)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("network.allowlist: %w", err)
	}
	return al, 0, nil
}

// validateImage enforces the "exactly one of Path/OCI" rule. v0.1 returns
// (501, stub error) for any set reference so callers get a clear signal
// rather than a silent fallback to the daemon default.
func validateImage(r *api.ImageRef) (status int, err error) {
	pathSet := r.Path != ""
	ociSet := r.OCI != ""
	switch {
	case pathSet && ociSet:
		return http.StatusBadRequest, errors.New("image.path and image.oci are mutually exclusive")
	case !pathSet && !ociSet:
		return http.StatusBadRequest, errors.New("image must set either path or oci")
	case ociSet:
		return http.StatusNotImplemented, errors.New("image.oci not implemented in v0.1 (tracked for wk7/wk8)")
	default: // pathSet only
		return http.StatusNotImplemented, errors.New("image.path per-sandbox override not implemented in v0.1 (use daemon --rootfs flag)")
	}
}

func sandboxResponseFrom(sb *sandbox.Sandbox) api.SandboxResponse {
	resp := api.SandboxResponse{
		ID:        sb.ID,
		VCPUs:     sb.VCPUs,
		MemoryMiB: sb.MemoryMiB,
		Workdir:   sb.Workdir,
		Profile:   sb.Profile,
		CreatedAt: sb.CreatedAt,
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

	// ImageRef validation — field is frozen as wire contract now but
	// any populated value returns a stub error in v0.1. See ImageRef.
	if req.Image != nil {
		status, err := validateImage(req.Image)
		writeError(w, status, err)
		return
	}

	// Network validation — parses the allowlist into a matcher or
	// rejects with 400. Passed into sandbox.Manager as a typed
	// NetworkConfig; nil means no NIC.
	var netCfg *sandbox.NetworkConfig
	if req.Network != nil {
		al, status, err := validateNetwork(req.Network)
		if err != nil {
			writeError(w, status, err)
			return
		}
		if al != nil {
			netCfg = &sandbox.NetworkConfig{Allowlist: al}
		}
	}

	// Scoped-token ceilings — every violation reported at once. max_sandboxes is
	// counted per token (CountByToken), so one token can't consume another's
	// budget (best-effort: the count races a concurrent create).
	tokenID := tokenIDFor(r)
	if pol := policyFor(r); pol != nil {
		var reqNet []string
		if req.Network != nil && req.Network.Enabled {
			reqNet = req.Network.Allowlist
		}
		if err := errors.Join(
			pol.CheckProfile(req.Profile),
			pol.CheckNetAllow(reqNet),
			pol.CheckVCPUs(req.VCPUs),
			pol.CheckMemory(req.MemoryMiB),
			pol.CheckCapacity(s.cfg.Manager.CountByToken(tokenID), 1),
		); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
	}

	sb, err := s.cfg.Manager.Create(r.Context(), sandbox.CreateConfig{
		VCPUs:      req.VCPUs,
		MemoryMiB:  req.MemoryMiB,
		BootArgs:   req.BootArgs,
		TimeoutSec: req.TimeoutSec,
		Profile:    req.Profile,
		Network:    netCfg,
		TokenID:    tokenID,
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrInvalidConfig) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
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
// body is a sequence of agentwire frames — identical in shape to what
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
	var req agentwire.ExecRequest
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
	fw := agentwire.NewFrameWriter(flushOnWrite{w: w, rc: rc})

	stdoutStream := fw.Stream(agentwire.FrameStdout)
	stderrStream := fw.Stream(agentwire.FrameStderr)

	result, err := s.cfg.Manager.Exec(r.Context(), id, req, stdoutStream, stderrStream)
	if err != nil {
		// Exec plumbing broke (agent unreachable, connection died,
		// etc.). Synthesize an exit frame so the client still sees a
		// well-formed stream.
		result = agentwire.ExecResult{ExitCode: -1, Error: err.Error()}
	}

	payload, jerr := json.Marshal(result)
	if jerr != nil {
		payload = []byte(fmt.Sprintf(`{"exit_code":-1,"error":%q}`, jerr.Error()))
	}
	_ = fw.WriteFrame(agentwire.FrameExit, payload)
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

	forks, err := s.cfg.Manager.Fork(r.Context(), id, count, tokenID)
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
	writeJSON(w, status, api.ErrorResponse{Error: err.Error()})
}
