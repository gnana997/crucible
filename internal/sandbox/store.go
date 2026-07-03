package sandbox

// Durable local authority (docs/GAPS.md gap 3).
//
// The Manager's registries live in memory; a daemon restart loses them
// while the on-disk workdirs, snapshot files, Firecracker processes,
// netns, and nft state persist. To reconcile on restart we persist a
// minimal record of every sandbox and snapshot to an append-only
// JSON-lines journal. Each line is one operation ("put" or "del"); the
// current record set is the replay of those lines. This is deliberately
// the simplest durable store that works — no embedded KV, no external
// dependency, no distributed anything. Reconcile compacts the journal
// back down to the surviving records on every startup so it can't grow
// without bound.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type recordKind string

const (
	kindSandbox  recordKind = "sandbox"
	kindSnapshot recordKind = "snapshot"
)

// journalEntry is one line in the journal. Exactly one of Sandbox /
// Snapshot is set on a "put"; a "del" carries only Kind + ID.
type journalEntry struct {
	Op       string          `json:"op"` // "put" | "del"
	Kind     recordKind      `json:"kind"`
	ID       string          `json:"id"`
	Sandbox  *sandboxRecord  `json:"sandbox,omitempty"`
	Snapshot *snapshotRecord `json:"snapshot,omitempty"`
}

// sandboxRecord is the persisted form of a live Sandbox — everything
// Reconcile needs to identify and reap its on-disk footprint. The live
// runner.Handle and agent client are deliberately NOT persisted: a
// restarted daemon can't re-attach to them, so persisted sandboxes are
// reaped on reconcile, not resurrected.
type sandboxRecord struct {
	ID        string         `json:"id"`
	VCPUs     int            `json:"vcpus"`
	MemoryMiB int            `json:"memory_mib"`
	Workdir   string         `json:"workdir"`
	VSockPath string         `json:"vsock_path,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Network   *networkRecord `json:"network,omitempty"`
}

// networkRecord captures a sandbox's network identity for logging and
// completeness. netns/nft/veth teardown itself is handled wholesale by
// network.ReapOrphans (which matches on the crucible- prefix), so these
// fields are informational — Reconcile does not replay them into live
// network state.
type networkRecord struct {
	NetnsPath string   `json:"netns_path,omitempty"`
	TapName   string   `json:"tap_name,omitempty"`
	GuestMAC  string   `json:"guest_mac,omitempty"`
	GuestIP   string   `json:"guest_ip,omitempty"`
	Gateway   string   `json:"gateway,omitempty"`
	Patterns  []string `json:"patterns,omitempty"`
}

// snapshotRecord is the persisted form of a Snapshot. Snapshots are pure
// on-disk artifacts (state/memory/rootfs files) with no live process, so
// Reconcile re-adopts them wholesale as long as their files still exist.
type snapshotRecord struct {
	ID              string    `json:"id"`
	SourceID        string    `json:"source_id"`
	VCPUs           int       `json:"vcpus"`
	MemoryMiB       int       `json:"memory_mib"`
	Dir             string    `json:"dir"`
	StatePath       string    `json:"state_path"`
	MemPath         string    `json:"mem_path"`
	RootfsPath      string    `json:"rootfs_path"`
	CreatedAt       time.Time `json:"created_at"`
	NetworkPatterns []string  `json:"network_patterns,omitempty"`
}

// store is an append-only JSON-lines journal with a fsync per write.
type store struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// openStore opens (creating if needed) the journal at path, replays it,
// and returns the current record set plus a store ready for appends.
// path's parent directory must already exist.
func openStore(path string) (*store, []sandboxRecord, []snapshotRecord, error) {
	sbx, snaps, err := replayJournal(path)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sandbox: open state journal: %w", err)
	}
	// If a prior crash left a torn (newline-less) final line, replay
	// already skipped it — but the append handle now sits right after
	// those bytes, so the next write would concatenate onto the partial
	// line and corrupt it too. Terminate the torn line first so appends
	// always start clean. (In the daemon Reconcile compacts the journal
	// anyway; this makes the store correct even if compaction is skipped
	// or fails.)
	if err := ensureTrailingNewline(f); err != nil {
		_ = f.Close()
		return nil, nil, nil, fmt.Errorf("sandbox: prepare state journal: %w", err)
	}
	return &store{path: path, f: f}, sbx, snaps, nil
}

// ensureTrailingNewline appends a '\n' when the file is non-empty and its
// last byte isn't already one. O_APPEND makes the Write land at EOF; the
// handle must be readable (O_RDWR) for the ReadAt peek.
func ensureTrailingNewline(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// replayJournal reads the whole journal and folds put/del operations into
// the current record set. A missing file means a fresh install. A torn
// final line (a crash mid-write) is tolerated: we skip unparseable lines
// rather than refuse to start.
func replayJournal(path string) ([]sandboxRecord, []snapshotRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: open state journal: %w", err)
	}
	defer func() { _ = f.Close() }()

	sbx := make(map[string]sandboxRecord)
	snaps := make(map[string]snapshotRecord)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn/garbled line — skip
		}
		switch e.Kind {
		case kindSandbox:
			if e.Op == "del" {
				delete(sbx, e.ID)
			} else if e.Sandbox != nil {
				sbx[e.Sandbox.ID] = *e.Sandbox
			}
		case kindSnapshot:
			if e.Op == "del" {
				delete(snaps, e.ID)
			} else if e.Snapshot != nil {
				snaps[e.Snapshot.ID] = *e.Snapshot
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("sandbox: read state journal: %w", err)
	}

	sbxOut := make([]sandboxRecord, 0, len(sbx))
	for _, r := range sbx {
		sbxOut = append(sbxOut, r)
	}
	snapOut := make([]snapshotRecord, 0, len(snaps))
	for _, r := range snaps {
		snapOut = append(snapOut, r)
	}
	return sbxOut, snapOut, nil
}

func (s *store) append(e journalEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return errors.New("sandbox: state journal closed")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		return err
	}
	return s.f.Sync()
}

func (s *store) putSandbox(r sandboxRecord) error {
	return s.append(journalEntry{Op: "put", Kind: kindSandbox, ID: r.ID, Sandbox: &r})
}

func (s *store) delSandbox(id string) error {
	return s.append(journalEntry{Op: "del", Kind: kindSandbox, ID: id})
}

func (s *store) putSnapshot(r snapshotRecord) error {
	return s.append(journalEntry{Op: "put", Kind: kindSnapshot, ID: r.ID, Snapshot: &r})
}

func (s *store) delSnapshot(id string) error {
	return s.append(journalEntry{Op: "del", Kind: kindSnapshot, ID: id})
}

// compact atomically rewrites the journal to contain exactly one "put"
// per surviving snapshot, discarding the delete history and any reaped
// sandbox records. Called once at the end of Reconcile so the journal
// can't grow across restarts. The write goes to a temp file that is
// fsync'd and renamed over the original; the append handle is then
// reopened on the compacted file.
func (s *store) compact(snaps []snapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("sandbox: compact journal: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := range snaps {
		r := snaps[i]
		if err := enc.Encode(journalEntry{Op: "put", Kind: kindSnapshot, ID: r.ID, Snapshot: &r}); err != nil {
			_ = f.Close()
			return fmt.Errorf("sandbox: compact journal: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sandbox: compact journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sandbox: compact journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sandbox: compact journal: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("sandbox: compact journal: %w", err)
	}

	if s.f != nil {
		_ = s.f.Close()
	}
	nf, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("sandbox: reopen compacted journal: %w", err)
	}
	s.f = nf
	return nil
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// sandboxRecordOf projects a live Sandbox to its persisted form.
func sandboxRecordOf(s *Sandbox) sandboxRecord {
	r := sandboxRecord{
		ID:        s.ID,
		VCPUs:     s.VCPUs,
		MemoryMiB: s.MemoryMiB,
		Workdir:   s.Workdir,
		VSockPath: s.VSockPath,
		CreatedAt: s.CreatedAt,
	}
	if s.Network != nil {
		nr := &networkRecord{
			NetnsPath: s.Network.NetnsPath,
			TapName:   s.Network.TapName,
			GuestMAC:  s.Network.GuestMAC,
			GuestIP:   s.Network.GuestIP,
			Gateway:   s.Network.Gateway,
		}
		if s.Network.Allowlist != nil {
			nr.Patterns = s.Network.Allowlist.Patterns()
		}
		r.Network = nr
	}
	return r
}

// snapshotRecordOf projects a live Snapshot to its persisted form.
func snapshotRecordOf(snap *Snapshot) snapshotRecord {
	r := snapshotRecord{
		ID:         snap.ID,
		SourceID:   snap.SourceID,
		VCPUs:      snap.VCPUs,
		MemoryMiB:  snap.MemoryMiB,
		Dir:        snap.Dir,
		StatePath:  snap.StatePath,
		MemPath:    snap.MemPath,
		RootfsPath: snap.RootfsPath,
		CreatedAt:  snap.CreatedAt,
	}
	if snap.Network != nil && snap.Network.Allowlist != nil {
		r.NetworkPatterns = snap.Network.Allowlist.Patterns()
	}
	return r
}
