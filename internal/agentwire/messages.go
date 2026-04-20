package agentwire

// ExecRequest is the JSON body of POST /exec. All fields except Cmd are
// optional; the agent fills in sensible defaults for the rest.
type ExecRequest struct {
	// Cmd is the argv. Cmd[0] is the executable (PATH-resolved inside the
	// guest), and Cmd[1:] are its arguments. Must be non-empty.
	Cmd []string `json:"cmd"`

	// Env is a map of environment variables to set. These are *added* to
	// the agent's environment rather than replacing it, so the command
	// inherits PATH / HOME / TERM from the agent unless explicitly
	// overridden here.
	Env map[string]string `json:"env,omitempty"`

	// Cwd is the working directory for the command. Empty means inherit
	// from the agent (typically /root or /).
	Cwd string `json:"cwd,omitempty"`

	// TimeoutSec is the command deadline in seconds. When it fires, the
	// agent sends SIGKILL to the process group and returns an ExecResult
	// with TimedOut=true. Zero means no deadline (the request's HTTP
	// connection is still bounded by the host's context).
	TimeoutSec int `json:"timeout_s,omitempty"`
}

// ExecResult is the payload of the terminal FrameExit frame on the
// streaming response. It describes how the command finished.
type ExecResult struct {
	// ExitCode is the process exit status. -1 when the process was killed
	// by a signal or never started.
	ExitCode int `json:"exit_code"`

	// DurationMs is wallclock time from agent-receives-request to
	// process-reaped, in milliseconds.
	DurationMs int64 `json:"duration_ms"`

	// Signal is the signal name when the process was killed by one, e.g.
	// "SIGKILL". Empty on clean exit.
	Signal string `json:"signal,omitempty"`

	// TimedOut reports whether the agent killed the process because
	// TimeoutSec elapsed.
	TimedOut bool `json:"timed_out,omitempty"`

	// Error is an agent-side failure string. Populated when the agent
	// could not start or reap the command (e.g. "command not found",
	// "permission denied"). When Error is non-empty, ExitCode is -1.
	Error string `json:"error,omitempty"`
}
