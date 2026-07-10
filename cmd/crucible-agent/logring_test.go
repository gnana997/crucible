//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/wire"
)

var ringT0 = time.Unix(1_700_000_000, 0)

func ringAppend(r *logRing, stream, s string) {
	r.append(stream, ringT0, []byte(s))
}

func TestLogRingAppendAndRead(t *testing.T) {
	r := newLogRing(1 << 20)
	ringAppend(r, wire.ServiceLogStdout, "hello\n")
	ringAppend(r, wire.ServiceLogStderr, "oops\n")

	resp := r.read(0, 1<<20)
	if len(resp.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(resp.Records))
	}
	if resp.Records[0].Stream != wire.ServiceLogStdout || string(resp.Records[0].Data) != "hello\n" {
		t.Errorf("rec0 = %+v", resp.Records[0])
	}
	if resp.Records[1].Stream != wire.ServiceLogStderr || string(resp.Records[1].Data) != "oops\n" {
		t.Errorf("rec1 = %+v", resp.Records[1])
	}
	if resp.Records[0].Seq != 0 || resp.Records[1].Seq != 1 {
		t.Errorf("seqs = %d,%d, want 0,1", resp.Records[0].Seq, resp.Records[1].Seq)
	}
	if resp.NextSeq != 2 || resp.FirstSeq != 0 {
		t.Errorf("NextSeq=%d FirstSeq=%d, want 2,0", resp.NextSeq, resp.FirstSeq)
	}
	if resp.DroppedRecords != 0 {
		t.Errorf("DroppedRecords = %d, want 0", resp.DroppedRecords)
	}
}

func TestLogRingCursorResume(t *testing.T) {
	r := newLogRing(1 << 20)
	ringAppend(r, wire.ServiceLogStdout, "one")
	ringAppend(r, wire.ServiceLogStdout, "two")

	first := r.read(0, 1<<20)
	if first.NextSeq != 2 {
		t.Fatalf("NextSeq = %d, want 2", first.NextSeq)
	}
	// Nothing new: empty page, same cursor.
	again := r.read(first.NextSeq, 1<<20)
	if len(again.Records) != 0 || again.NextSeq != 2 {
		t.Fatalf("read at cursor = %d records, NextSeq %d", len(again.Records), again.NextSeq)
	}
	ringAppend(r, wire.ServiceLogStdout, "three")
	resumed := r.read(first.NextSeq, 1<<20)
	if len(resumed.Records) != 1 || string(resumed.Records[0].Data) != "three" {
		t.Fatalf("resume read = %+v", resumed.Records)
	}
}

func TestLogRingMaxBytesPaging(t *testing.T) {
	r := newLogRing(1 << 20)
	for i := 0; i < 10; i++ {
		ringAppend(r, wire.ServiceLogStdout, strings.Repeat("x", 100))
	}
	var got int
	cursor := uint64(0)
	for pages := 0; pages < 20; pages++ {
		resp := r.read(cursor, 250)
		got += len(resp.Records)
		if resp.NextSeq == cursor {
			break
		}
		cursor = resp.NextSeq
	}
	if got != 10 {
		t.Fatalf("paged read returned %d records, want 10", got)
	}
}

func TestLogRingOversizedWriteIsChunked(t *testing.T) {
	r := newLogRing(1 << 20)
	big := bytes.Repeat([]byte("a"), logChunkMax*2+10)
	r.append(wire.ServiceLogStdout, ringT0, big)

	resp := r.read(0, 1<<20)
	if len(resp.Records) != 3 {
		t.Fatalf("records = %d, want 3 chunks", len(resp.Records))
	}
	var total int
	for _, rec := range resp.Records {
		total += len(rec.Data)
	}
	if total != len(big) {
		t.Errorf("total bytes = %d, want %d", total, len(big))
	}
	// A single record larger than max_bytes must still make progress.
	one := r.read(0, 10)
	if len(one.Records) != 1 {
		t.Fatalf("tiny read = %d records, want 1 (progress guarantee)", len(one.Records))
	}
}

func TestLogRingEvictionIsExplicit(t *testing.T) {
	// Budget for roughly 3 records of 100 bytes + overhead.
	r := newLogRing(3 * (100 + logRecordOverhead))
	for i := 0; i < 10; i++ {
		ringAppend(r, wire.ServiceLogStdout, strings.Repeat("x", 100))
	}
	resp := r.read(0, 1<<20)
	if resp.DroppedRecords == 0 || resp.DroppedBytes == 0 {
		t.Fatal("eviction not reported")
	}
	if resp.FirstSeq == 0 {
		t.Fatal("FirstSeq = 0 after eviction, gap invisible")
	}
	if uint64(len(resp.Records))+resp.DroppedRecords != 10 {
		t.Errorf("records %d + dropped %d != 10", len(resp.Records), resp.DroppedRecords)
	}
	// Records that survive are the newest ones, contiguous to NextSeq.
	if last := resp.Records[len(resp.Records)-1].Seq; last != resp.NextSeq-1 {
		t.Errorf("last seq = %d, NextSeq = %d", last, resp.NextSeq)
	}
}

func TestLogRingCompaction(t *testing.T) {
	// Tiny budget forces constant eviction; the backing slice must not
	// grow proportionally to total writes.
	r := newLogRing(2 * (100 + logRecordOverhead))
	for i := 0; i < 10_000; i++ {
		ringAppend(r, wire.ServiceLogStdout, strings.Repeat("y", 100))
	}
	r.mu.Lock()
	backing := len(r.records)
	r.mu.Unlock()
	if backing > 1024 {
		t.Fatalf("backing slice = %d records after 10k writes with tiny budget", backing)
	}
	resp := r.read(0, 1<<20)
	if resp.NextSeq != 10_000 {
		t.Errorf("NextSeq = %d, want 10000", resp.NextSeq)
	}
}

func TestLogRingStats(t *testing.T) {
	r := newLogRing(1 << 20)
	first, next, dropped := r.stats()
	if first != 0 || next != 0 || dropped != 0 {
		t.Fatalf("empty ring stats = %d,%d,%d", first, next, dropped)
	}
	ringAppend(r, wire.ServiceLogStdout, "a")
	first, next, _ = r.stats()
	if first != 0 || next != 1 {
		t.Fatalf("stats after one append = %d,%d, want 0,1", first, next)
	}
}
