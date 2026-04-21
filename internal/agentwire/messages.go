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

	// OomKilled is a best-effort heuristic: true when the process was
	// killed by SIGKILL (and not by us via timeout) AND its peak RSS
	// reached at least 95% of the guest's total memory. Close to
	// correct in practice but not bulletproof — a precise answer
	// requires per-exec cgroup v2 memory.events, which lands with
	// per-exec cgroups in a later weekend.
	OomKilled bool `json:"oom_killed,omitempty"`

	// Error is an agent-side failure string. Populated when the agent
	// could not start or reap the command (e.g. "command not found",
	// "permission denied"). When Error is non-empty, ExitCode is -1.
	Error string `json:"error,omitempty"`

	// Usage carries CPU, memory, and I/O counters for this exec.
	// Nil when the agent didn't collect stats (e.g. the process
	// couldn't be started) so callers can distinguish "no data"
	// from "zeroes". All fields use explicit SI units in their name
	// so clients don't have to guess.
	Usage *ResourceUsage `json:"usage,omitempty"`
}

// ResourceUsage captures per-exec resource counters. Most fields come
// from wait4's Rusage via ProcessState.SysUsage; the I/O counters
// come from polling /proc/<pid>/io while the child runs.
//
// All counters are cumulative for the lifetime of the direct child.
// Grandchildren reaped by the child are included in CPU/memory stats
// (kernel accounting) but NOT in I/O stats (Linux's /proc/<pid>/io
// is per-process). Full process-tree accounting requires per-exec
// cgroups, which we don't set up in v0.1.
type ResourceUsage struct {
	// CPUUserMs is user-land CPU time consumed by the process.
	CPUUserMs int64 `json:"cpu_user_ms"`

	// CPUSysMs is kernel CPU time consumed on behalf of the process.
	CPUSysMs int64 `json:"cpu_sys_ms"`

	// PeakMemoryBytes is the largest resident set size observed
	// during exec. Derived from Rusage.Maxrss (Linux reports it in
	// kilobytes; we multiply by 1024 before shipping).
	PeakMemoryBytes int64 `json:"peak_memory_bytes"`

	// PageFaultsMajor is the number of hard page faults — faults
	// that required reading a page from backing storage. A high
	// number correlates with disk-bound workloads.
	PageFaultsMajor int64 `json:"page_faults_major"`

	// ContextSwitchesInvoluntary is the number of times the process
	// was preempted by the scheduler (as opposed to yielding
	// voluntarily via a blocking syscall). High values indicate CPU
	// contention — either with other tenants or with host work.
	ContextSwitchesInvoluntary int64 `json:"context_switches_involuntary"`

	// IOReadBytes is the number of bytes actually read from the
	// storage layer (post-page-cache) by this process, per
	// /proc/<pid>/io read_bytes.
	//
	// Note on accuracy: the agent polls /proc/<pid>/io at a fixed
	// cadence while the process is alive. For very short-lived
	// processes (tens of milliseconds) the last poll may miss I/O
	// that happened between the most recent read and process exit.
	// Treat these counters as approximate for sub-100ms commands.
	IOReadBytes int64 `json:"io_read_bytes"`

	// IOWriteBytes is the mirror of IOReadBytes for writes.
	IOWriteBytes int64 `json:"io_write_bytes"`
}
