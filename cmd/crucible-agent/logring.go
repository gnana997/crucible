//go:build linux

package main

import (
	"io"
	"sync"
	"time"

	"github.com/gnana997/crucible/sdk/wire"
)

// logChunkMax bounds one ring record's payload. Larger writes are split;
// chunk boundaries follow the child's write pattern, not lines.
const logChunkMax = 4096

// logRecordOverhead is the per-record byte cost charged against the
// ring budget on top of the payload, approximating the Go-side struct +
// slice header footprint so budget ~= real memory.
const logRecordOverhead = 64

// logRing is the in-memory stdout/stderr capture for one service: a
// FIFO of records with a byte budget. Appends evict from the head;
// evictions are counted so readers always see an explicit gap rather
// than a silent hole. Safe for concurrent use — the two pipe-drain
// goroutines write while HTTP readers read.
type logRing struct {
	mu      sync.Mutex
	budget  int
	used    int
	head    int // index of the oldest record in records
	records []logRecord

	nextSeq        uint64
	droppedRecords uint64
	droppedBytes   uint64
}

type logRecord struct {
	seq    uint64
	stream string
	at     time.Time
	data   []byte
}

func newLogRing(budget int) *logRing {
	return &logRing{budget: budget}
}

// append stores one output chunk, splitting oversized writes and
// evicting from the head to stay within budget.
func (r *logRing) append(stream string, at time.Time, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(p) > 0 {
		n := min(len(p), logChunkMax)
		chunk := make([]byte, n)
		copy(chunk, p[:n])
		p = p[n:]

		r.records = append(r.records, logRecord{seq: r.nextSeq, stream: stream, at: at, data: chunk})
		r.nextSeq++
		r.used += n + logRecordOverhead

		for r.used > r.budget && r.head < len(r.records)-1 {
			old := &r.records[r.head]
			r.used -= len(old.data) + logRecordOverhead
			r.droppedBytes += uint64(len(old.data))
			r.droppedRecords++
			old.data = nil // release the payload while the slot waits for compaction
			r.head++
		}
		// Compact the slice once the dead prefix dominates, so memory
		// stays proportional to the budget, not to history.
		if r.head > 64 && r.head*2 > len(r.records) {
			r.records = append(r.records[:0], r.records[r.head:]...)
			r.head = 0
		}
	}
}

// read returns records from fromSeq (clamped to the oldest available),
// oldest first, up to maxBytes of payload. It never blocks.
func (r *logRing) read(fromSeq uint64, maxBytes int) wire.ServiceLogsResponse {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := wire.ServiceLogsResponse{
		NextSeq:        r.nextSeq,
		FirstSeq:       r.firstSeqLocked(),
		DroppedRecords: r.droppedRecords,
		DroppedBytes:   r.droppedBytes,
	}
	if fromSeq < resp.FirstSeq {
		fromSeq = resp.FirstSeq // the gap is visible via FirstSeq
	}

	total := 0
	for i := r.head; i < len(r.records); i++ {
		rec := &r.records[i]
		if rec.seq < fromSeq {
			continue
		}
		if total+len(rec.data) > maxBytes && len(resp.Records) > 0 {
			resp.NextSeq = rec.seq
			return resp
		}
		resp.Records = append(resp.Records, wire.ServiceLogRecord{
			Seq:    rec.seq,
			Stream: rec.stream,
			UnixMs: rec.at.UnixMilli(),
			Data:   rec.data,
		})
		total += len(rec.data)
	}
	return resp
}

// stats reports cursor bounds for ServiceStatus.
func (r *logRing) stats() (firstSeq, nextSeq, droppedBytes uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstSeqLocked(), r.nextSeq, r.droppedBytes
}

func (r *logRing) firstSeqLocked() uint64 {
	if r.head < len(r.records) {
		return r.records[r.head].seq
	}
	return r.nextSeq
}

// ringWriter adapts one stream of the ring to io.Writer for os/exec.
type ringWriter struct {
	ring   *logRing
	stream string
	now    func() time.Time
}

var _ io.Writer = ringWriter{}

func (w ringWriter) Write(p []byte) (int, error) {
	w.ring.append(w.stream, w.now(), p)
	return len(p), nil
}
