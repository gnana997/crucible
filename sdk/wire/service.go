package wire

import (
	"errors"
	"fmt"
	"strings"
)

// Service lifecycle states, as reported in ServiceStatus.State. The
// supervisor inside the agent owns the state machine; these strings are
// the wire contract.
const (
	// ServiceStateIdle: no service spec has been configured yet (or the
	// agent restarted and lost it — the host must re-configure).
	ServiceStateIdle = "idle"
	// ServiceStateStarting: the supervisor is launching the process.
	// Transient; external observers rarely see it.
	ServiceStateStarting = "starting"
	// ServiceStateRunning: the entrypoint process is alive.
	ServiceStateRunning = "running"
	// ServiceStateStopping: a stop was requested; the process has been
	// signalled and the supervisor is waiting for it to exit (grace
	// period, then SIGKILL).
	ServiceStateStopping = "stopping"
	// ServiceStateBackingOff: the process exited and the restart policy
	// scheduled a relaunch after a backoff delay.
	ServiceStateBackingOff = "backing_off"
	// ServiceStateStopped: the process exited and no restart is
	// scheduled (explicit stop, or policy chose not to restart).
	ServiceStateStopped = "stopped"
	// ServiceStateFailed: the process could not be started, or the
	// on-failure retry budget is exhausted. Terminal until the host
	// intervenes (start/restart/configure).
	ServiceStateFailed = "failed"
)

// Restart policy names. Semantics mirror Docker's restart policies so
// the operator mental model transfers unchanged.
const (
	// RestartNever: never restart automatically. The default.
	RestartNever = "never"
	// RestartOnFailure: restart only when the process exits non-zero
	// (or by signal), up to MaxRetries consecutive failures.
	RestartOnFailure = "on-failure"
	// RestartAlways: restart on any exit, clean or not.
	RestartAlways = "always"
)

// ServiceSpec defaults and caps.
const (
	// DefaultStopSignal is sent to the process group on stop when the
	// spec doesn't name one.
	DefaultStopSignal = "SIGTERM"
	// DefaultStopGraceSec is how long the supervisor waits between the
	// stop signal and SIGKILL when the spec doesn't say. Matches
	// Docker's default stop timeout.
	DefaultStopGraceSec = 10
	// DefaultLogBufferBytes is the in-memory log ring budget per
	// service when the spec doesn't set one.
	DefaultLogBufferBytes = 2 << 20 // 2 MiB
	// MaxLogBufferBytes caps a spec-requested log ring so a request
	// can't balloon agent memory.
	MaxLogBufferBytes = 16 << 20 // 16 MiB
)

// RestartPolicy tells the supervisor what to do when the service
// process exits on its own. Explicit stop/restart commands are never
// subject to policy.
type RestartPolicy struct {
	// Policy is one of RestartNever, RestartOnFailure, RestartAlways.
	// Empty means RestartNever.
	Policy string `json:"policy,omitempty"`

	// MaxRetries bounds consecutive on-failure restarts; when the
	// budget is exhausted the service enters the failed state. Zero
	// means unlimited. Only valid with RestartOnFailure (mirroring
	// docker run --restart=on-failure:N).
	MaxRetries int `json:"max_retries,omitempty"`
}

// ServiceSpec is the JSON body of PUT /service: the long-lived
// entrypoint the agent supervises. Cmd/Env/Cwd carry the same
// semantics as ExecRequest; there is deliberately no timeout — a
// service has no deadline, it runs until stopped or it exits.
type ServiceSpec struct {
	// Cmd is the argv. Cmd[0] is the executable (PATH-resolved inside
	// the guest). Must be non-empty.
	Cmd []string `json:"cmd"`

	// Env is the environment. In the default (profile) mode it is added
	// to the agent's environment; with EnvExact it is the complete
	// environment (see EnvExact).
	Env map[string]string `json:"env,omitempty"`

	// Cwd is the working directory. Empty means inherit from the agent.
	// With EnvExact the agent creates it if absent (Docker WORKDIR
	// semantics).
	Cwd string `json:"cwd,omitempty"`

	// User is the OCI user the process runs as: "", "user", "uid",
	// "user:group", "uid:gid", "uid:group", or "user:gid". Empty runs
	// as the agent's user (root). Resolved against the guest's
	// /etc/passwd + /etc/group at launch — the daemon computes it from
	// the image config but never resolves it (only the guest has those
	// files).
	User string `json:"user,omitempty"`

	// EnvExact selects Docker-faithful environment handling for OCI
	// images: the process gets exactly Env (no agent-environ merge),
	// plus Docker's default PATH when Env sets none and HOME from the
	// resolved user. False (the default, profile services) keeps the
	// merge-with-agent-environ behavior.
	EnvExact bool `json:"env_exact,omitempty"`

	// StopSignal is the signal name sent to the process group on stop,
	// e.g. "SIGTERM" or "SIGINT". Empty means DefaultStopSignal. The
	// agent resolves and rejects unknown names at configure time.
	StopSignal string `json:"stop_signal,omitempty"`

	// StopGraceSec is the delay between StopSignal and SIGKILL on stop.
	// Zero means DefaultStopGraceSec.
	StopGraceSec int `json:"stop_grace_s,omitempty"`

	// Restart is the automatic restart policy. Zero value means never.
	Restart RestartPolicy `json:"restart"`

	// LogBufferBytes is the in-memory log ring budget for this
	// service's stdout/stderr. Zero means DefaultLogBufferBytes;
	// values above MaxLogBufferBytes are rejected.
	LogBufferBytes int `json:"log_buffer_bytes,omitempty"`
}

// Normalize fills defaulted fields in place. Call after Validate.
func (s *ServiceSpec) Normalize() {
	if s.StopSignal == "" {
		s.StopSignal = DefaultStopSignal
	}
	if s.StopGraceSec == 0 {
		s.StopGraceSec = DefaultStopGraceSec
	}
	if s.Restart.Policy == "" {
		s.Restart.Policy = RestartNever
	}
	if s.LogBufferBytes == 0 {
		s.LogBufferBytes = DefaultLogBufferBytes
	}
}

// Validate checks the structural rules a spec must satisfy regardless
// of where it is evaluated (daemon or agent). Signal-name resolution
// is deliberately not here: it needs the platform signal table, so the
// agent does it at configure time.
func (s *ServiceSpec) Validate() error {
	if len(s.Cmd) == 0 {
		return errors.New("service: cmd is required")
	}
	if s.Cmd[0] == "" {
		return errors.New("service: cmd[0] must not be empty")
	}
	if err := validateUser(s.User); err != nil {
		return err
	}
	if s.StopGraceSec < 0 {
		return errors.New("service: stop_grace_s must be >= 0")
	}
	switch s.Restart.Policy {
	case "", RestartNever, RestartAlways:
		if s.Restart.MaxRetries != 0 {
			return fmt.Errorf("service: max_retries is only valid with restart policy %q", RestartOnFailure)
		}
	case RestartOnFailure:
		if s.Restart.MaxRetries < 0 {
			return errors.New("service: max_retries must be >= 0")
		}
	default:
		return fmt.Errorf("service: unknown restart policy %q", s.Restart.Policy)
	}
	if s.LogBufferBytes < 0 {
		return errors.New("service: log_buffer_bytes must be >= 0")
	}
	if s.LogBufferBytes > MaxLogBufferBytes {
		return fmt.Errorf("service: log_buffer_bytes exceeds the %d-byte cap", MaxLogBufferBytes)
	}
	return nil
}

// validateUser checks the OCI user string's shape (at most one colon,
// non-empty parts). Resolution against passwd/group is the guest's job.
func validateUser(u string) error {
	if u == "" {
		return nil
	}
	user, group, hasColon := strings.Cut(u, ":")
	if user == "" {
		return errors.New("service: user must not be empty before ':'")
	}
	if hasColon && group == "" {
		return errors.New("service: group must not be empty after ':'")
	}
	if strings.Contains(group, ":") {
		return errors.New("service: user must have at most one ':'")
	}
	return nil
}

// ServiceStatus is the JSON body of GET /service/status responses (and
// of every service mutation response, so callers always see the state
// they produced).
type ServiceStatus struct {
	// State is one of the ServiceState* constants.
	State string `json:"state"`

	// Pid is the entrypoint's process id while running/stopping, else 0.
	Pid int `json:"pid,omitempty"`

	// StartedAtUnixMs is when the current process was launched (wall
	// clock, guest view). Zero when no process is running. Note the
	// guest wall clock is stale immediately after a snapshot restore.
	StartedAtUnixMs int64 `json:"started_at_unix_ms,omitempty"`

	// UptimeMs is how long the current process has been running.
	UptimeMs int64 `json:"uptime_ms,omitempty"`

	// Restarts counts automatic policy restarts since the last explicit
	// start/restart/configure command.
	Restarts int `json:"restarts,omitempty"`

	// Spec echoes the configured (normalized) spec, nil when idle.
	Spec *ServiceSpec `json:"spec,omitempty"`

	// LastExit describes how the previous process finished. Signal
	// deaths report ExitCode 128+signal (Docker convention) alongside
	// the signal name.
	LastExit *ExecResult `json:"last_exit,omitempty"`

	// LastExitRequested is true when the previous exit was caused by a
	// stop/restart/configure command rather than the process dying on
	// its own.
	LastExitRequested bool `json:"last_exit_requested,omitempty"`

	// LastExitAtUnixMs is when the previous process exited.
	LastExitAtUnixMs int64 `json:"last_exit_at_unix_ms,omitempty"`

	// LiveRSSBytes / LivePeakRSSBytes are a best-effort read of the
	// running process's current and peak resident set (from
	// /proc/<pid>/status). Zero when unavailable or not running.
	LiveRSSBytes     int64 `json:"live_rss_bytes,omitempty"`
	LivePeakRSSBytes int64 `json:"live_peak_rss_bytes,omitempty"`

	// Log ring cursor bounds: the oldest available and next sequence
	// numbers, plus total bytes evicted so far. A host-side shipper
	// resumes from its own cursor and detects loss by comparing it to
	// LogFirstSeq. All zero until a spec is configured.
	LogFirstSeq     uint64 `json:"log_first_seq,omitempty"`
	LogNextSeq      uint64 `json:"log_next_seq,omitempty"`
	LogDroppedBytes uint64 `json:"log_dropped_bytes,omitempty"`
}

// ServiceStopRequest is the optional JSON body of POST /service/stop.
type ServiceStopRequest struct {
	// GraceSec overrides the spec's StopGraceSec for this stop only.
	// Zero means use the spec's value.
	GraceSec int `json:"grace_s,omitempty"`
}

// Log stream names used in ServiceLogRecord.Stream.
const (
	ServiceLogStdout = "stdout"
	ServiceLogStderr = "stderr"
)

// ServiceLogRecord is one captured chunk of the service's output.
type ServiceLogRecord struct {
	// Seq is the record's monotonic sequence number. Sequence numbers
	// are dense: a reader that sees seq jump past its cursor has hit
	// ring eviction (see ServiceLogsResponse.FirstSeq).
	Seq uint64 `json:"seq"`

	// Stream is ServiceLogStdout or ServiceLogStderr.
	Stream string `json:"stream"`

	// UnixMs is the guest wall-clock time the chunk was captured. Note
	// the guest clock is stale right after a snapshot restore.
	UnixMs int64 `json:"unix_ms"`

	// Data is the raw output chunk (base64 on the wire via
	// encoding/json). Chunk boundaries are arbitrary — they follow the
	// child's write pattern, not lines.
	Data []byte `json:"data"`
}

// ServiceLogsResponse is the JSON body of GET /service/logs.
type ServiceLogsResponse struct {
	// Records are the chunks from the requested cursor, oldest first,
	// bounded by the request's max_bytes.
	Records []ServiceLogRecord `json:"records"`

	// NextSeq is the cursor to pass as from_seq on the next read.
	NextSeq uint64 `json:"next_seq"`

	// FirstSeq is the oldest sequence number still in the ring. A
	// caller whose from_seq is older than this has lost
	// (FirstSeq - from_seq) records to eviction — an explicit gap,
	// never a silent hole.
	FirstSeq uint64 `json:"first_seq"`

	// DroppedRecords / DroppedBytes count everything evicted from the
	// ring since the service was configured.
	DroppedRecords uint64 `json:"dropped_records,omitempty"`
	DroppedBytes   uint64 `json:"dropped_bytes,omitempty"`
}
