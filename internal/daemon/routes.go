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
	return mux
}

// --- request / response DTOs -----------------------------------------

// createSandboxRequest is the JSON body for POST /sandboxes. All fields
// are optional; sandbox.Manager fills in defaults for zero values.
type createSandboxRequest struct {
	VCPUs     int    `json:"vcpus,omitempty"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	BootArgs  string `json:"boot_args,omitempty"`
	// TimeoutSec sets a maximum lifetime for the sandbox in seconds.
	// Zero means no timeout; the sandbox lives until an explicit
	// DELETE or daemon shutdown.
	TimeoutSec int `json:"timeout_s,omitempty"`

	// Image, when set, overrides the daemon's default rootfs for this
	// sandbox. See ImageRef for the field shape.
	//
	// WIRE CONTRACT LOCK (wk3): the shape is frozen now so the OCI
	// client landing in wk7/wk8 doesn't force a breaking API change.
	// For v0.1 both Path and OCI return 501 — callers must leave
	// Image unset and rely on daemon-level --rootfs.
	Image *ImageRef `json:"image,omitempty"`
}

// ImageRef identifies the rootfs to mount into a sandbox. Exactly one
// of Path / OCI must be set; neither-set and both-set are invalid.
//
//   - Path is an absolute host path to a pre-built ext4 image. Same
//     semantics as the daemon's --rootfs flag, but per-sandbox.
//   - OCI is an OCI image reference (e.g. "ghcr.io/you/python:3.12").
//     The daemon will pull + convert it to ext4 at handler time.
//
// Neither is implemented in v0.1 — the field lives here as a wire
// contract so later weekends can add behavior without breaking
// existing clients.
type ImageRef struct {
	Path string `json:"path,omitempty"`
	OCI  string `json:"oci,omitempty"`
}

// validate enforces the "exactly one of Path/OCI" rule. v0.1 returns
// (501, <stub error>) for any set reference so callers get a clear
// signal rather than silent fallback to the daemon default.
func (r *ImageRef) validate() (status int, err error) {
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

// sandboxResponse is the JSON shape returned for a single sandbox. Kept
// separate from sandbox.Sandbox so the daemon can shape the public
// surface without coupling to the manager's internal struct.
type sandboxResponse struct {
	ID        string    `json:"id"`
	VCPUs     int       `json:"vcpus"`
	MemoryMiB int       `json:"memory_mib"`
	Workdir   string    `json:"workdir"`
	CreatedAt time.Time `json:"created_at"`
}

func sandboxResponseFrom(sb *sandbox.Sandbox) sandboxResponse {
	return sandboxResponse{
		ID:        sb.ID,
		VCPUs:     sb.VCPUs,
		MemoryMiB: sb.MemoryMiB,
		Workdir:   sb.Workdir,
		CreatedAt: sb.CreatedAt,
	}
}

// listResponse wraps the sandbox list so the response shape can grow
// without breaking clients (e.g. adding "next_page" later).
type listResponse struct {
	Sandboxes []sandboxResponse `json:"sandboxes"`
}

// snapshotResponse is the JSON shape returned for a single snapshot.
// The on-disk paths are included because they're useful for operator
// debugging (ls, debugfs, du) and don't leak any secrets.
type snapshotResponse struct {
	ID         string    `json:"id"`
	SourceID   string    `json:"source_id"`
	VCPUs      int       `json:"vcpus"`
	MemoryMiB  int       `json:"memory_mib"`
	Dir        string    `json:"dir"`
	StatePath  string    `json:"state_path"`
	MemPath    string    `json:"mem_path"`
	RootfsPath string    `json:"rootfs_path"`
	CreatedAt  time.Time `json:"created_at"`
}

func snapshotResponseFrom(s *sandbox.Snapshot) snapshotResponse {
	return snapshotResponse{
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

type snapshotListResponse struct {
	Snapshots []snapshotResponse `json:"snapshots"`
}

type forkResponse struct {
	Sandboxes []sandboxResponse `json:"sandboxes"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// --- handlers --------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	// Cap request body so a malicious client can't exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	var req createSandboxRequest
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
		status, err := req.Image.validate()
		writeError(w, status, err)
		return
	}

	sb, err := s.cfg.Manager.Create(r.Context(), sandbox.CreateConfig{
		VCPUs:      req.VCPUs,
		MemoryMiB:  req.MemoryMiB,
		BootArgs:   req.BootArgs,
		TimeoutSec: req.TimeoutSec,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, sandboxResponseFrom(sb))
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, _ *http.Request) {
	all := s.cfg.Manager.List()
	out := make([]sandboxResponse, 0, len(all))
	for _, sb := range all {
		out = append(out, sandboxResponseFrom(sb))
	}
	writeJSON(w, http.StatusOK, listResponse{Sandboxes: out})
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

	// Commit to streaming.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	fw := agentwire.NewFrameWriter(flushOnWrite{w: w, flusher: flusher})

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
	out := make([]snapshotResponse, 0, len(all))
	for _, snap := range all {
		out = append(out, snapshotResponseFrom(snap))
	}
	writeJSON(w, http.StatusOK, snapshotListResponse{Snapshots: out})
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

	forks, err := s.cfg.Manager.Fork(r.Context(), id, count)
	if err != nil {
		if errors.Is(err, sandbox.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]sandboxResponse, 0, len(forks))
	for _, f := range forks {
		out = append(out, sandboxResponseFrom(f))
	}
	writeJSON(w, http.StatusCreated, forkResponse{Sandboxes: out})
}

// flushOnWrite forwards every Write to w and flushes the underlying
// chunked response so frames appear on the wire byte-for-byte as the
// agent produces them. Without the Flush the stdlib would buffer and
// the client would only see output when the command exits.
type flushOnWrite struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (f flushOnWrite) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil && f.flusher != nil {
		f.flusher.Flush()
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
	writeJSON(w, status, errorResponse{Error: err.Error()})
}
