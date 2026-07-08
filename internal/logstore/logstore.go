// Package logstore persists per-sandbox logs as durable, tailable
// append-only NDJSON files. It is pure persistence: the daemon feeds it
// two sources — the service's captured stdout/stderr (drained from the
// guest ring) and exec activity — and reads it back for `crucible logs`.
//
// One file per sandbox, `<dir>/<sandboxID>.log`, one JSON record per
// line. The read cursor is a byte offset, so following is a stateless
// poll ("give me records after offset N") with no server-side follower
// machinery — the same cursor model the guest log ring already uses.
package logstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Source values for Record.Source.
const (
	SourceService = "service" // the supervised entrypoint's output
	SourceExec    = "exec"    // output/activity of an exec invocation
)

// Stream values for Record.Stream.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamEvent  = "event" // a synthesized line (exec start/exit, ring gap)
)

// Record is one persisted log line, stored as compact NDJSON. Callers
// supply TimeMs (from the guest ring for service records, or wall-clock
// for exec) — logstore keeps no clock of its own.
type Record struct {
	TimeMs int64  `json:"t"`
	Source string `json:"src"`
	Stream string `json:"s"`
	Text   string `json:"m"`
}

// defaultMaxFileBytes caps a single sandbox's log file. Past it we drop
// further writes after a one-time marker — a cheap guard against a
// runaway app filling the disk. Intra-file rotation is a later concern
// (it matters once apps run indefinitely; sandboxes are ephemeral today).
const defaultMaxFileBytes int64 = 64 << 20

// Store owns the per-sandbox log files under Dir.
type Store struct {
	dir          string
	maxFileBytes int64

	mu    sync.Mutex
	files map[string]*sandboxLog
}

// sandboxLog is one sandbox's open append handle plus its running size.
type sandboxLog struct {
	mu        sync.Mutex
	f         *os.File
	size      int64
	truncated bool
}

// New creates a Store rooted at dir, creating the directory if needed.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("logstore: dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("logstore: create dir: %w", err)
	}
	return &Store{dir: dir, maxFileBytes: defaultMaxFileBytes, files: map[string]*sandboxLog{}}, nil
}

// validID rejects anything that could escape the log directory. Sandbox
// IDs are already validated at the HTTP boundary; this is defense in depth.
func validID(id string) bool {
	return id != "" && id == filepath.Base(id) &&
		id != "." && id != ".." && !strings.ContainsRune(id, os.PathSeparator)
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".log") }

// logFor returns the sandbox's open append handle, opening it on first use.
func (s *Store) logFor(id string) (*sandboxLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.files[id]; ok {
		return l, nil
	}
	f, err := os.OpenFile(s.path(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("logstore: open %s: %w", id, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l := &sandboxLog{f: f, size: info.Size()}
	s.files[id] = l
	return l, nil
}

// Append writes one record to a sandbox's log.
func (s *Store) Append(id string, rec Record) error {
	if !validID(id) {
		return fmt.Errorf("logstore: invalid sandbox id %q", id)
	}
	l, err := s.logFor(id)
	if err != nil {
		return err
	}
	return l.append(rec, s.maxFileBytes)
}

func (l *sandboxLog) append(rec Record, maxBytes int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if maxBytes > 0 && l.size+int64(len(line)) > maxBytes {
		marker, _ := json.Marshal(Record{
			TimeMs: rec.TimeMs, Source: rec.Source, Stream: StreamEvent,
			Text: "[log truncated: max size reached]",
		})
		if n, werr := l.f.Write(append(marker, '\n')); werr == nil {
			l.size += int64(n)
		}
		l.truncated = true
		return nil
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	return err
}

// Read returns records from a sandbox's log. The cursor is a byte offset:
//   - since < 0  → tail: the last tailBytes worth of complete records.
//   - since >= 0 → records starting at that offset (for a follow poll).
//
// It returns the records (at most max, 0 = unlimited) and next: the byte
// offset just past the last complete record returned, to pass as `since`
// on the next poll. A missing log (sandbox produced none yet) is not an
// error — it returns (nil, 0, nil).
func (s *Store) Read(id string, since int64, tailBytes, max int) (recs []Record, next int64, err error) {
	if !validID(id) {
		return nil, 0, fmt.Errorf("logstore: invalid sandbox id %q", id)
	}
	f, err := os.Open(s.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()

	start := since
	tailing := since < 0
	if tailing {
		start = size - int64(tailBytes)
		if start < 0 {
			start = 0
		}
	}
	if start > size {
		start = size
	}

	data := make([]byte, size-start)
	if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, start, err
	}

	i := 0
	// When tailing from a mid-file offset, the first line is almost
	// certainly partial — skip to just after the first newline.
	if tailing && start > 0 {
		if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
			i = nl + 1
		} else {
			i = len(data)
		}
	}
	next = start + int64(i)
	for i < len(data) {
		nl := bytes.IndexByte(data[i:], '\n')
		if nl < 0 {
			break // incomplete trailing line — leave next before it
		}
		line := data[i : i+nl]
		i += nl + 1
		lineEnd := start + int64(i)
		var rec Record
		if json.Unmarshal(line, &rec) == nil {
			recs = append(recs, rec)
		}
		next = lineEnd
		if max > 0 && len(recs) >= max {
			break
		}
	}
	return recs, next, nil
}

// Close closes every open append handle. Reads reopen the file, so this
// only affects writers; call it on daemon shutdown.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.files {
		l.mu.Lock()
		_ = l.f.Close()
		l.mu.Unlock()
	}
	s.files = map[string]*sandboxLog{}
	return nil
}
