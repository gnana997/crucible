package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
