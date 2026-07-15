package appevents

import (
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	t := time.Unix(0, 0).UTC()
	return func() time.Time { return t }
}

func TestEmitAssignsMonotonicSeqAndTime(t *testing.T) {
	s := newStore(8, fixedClock())
	for i := 0; i < 3; i++ {
		s.Emit(AppEvent{Type: TypeCreated, App: "web"})
	}
	evs, cursor := s.Since(0)
	if len(evs) != 3 || cursor != 3 {
		t.Fatalf("Since(0) = %d events, cursor %d; want 3/3", len(evs), cursor)
	}
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d Seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Time.IsZero() {
			t.Errorf("event %d has zero Time", i)
		}
	}
}

func TestSinceCursor(t *testing.T) {
	s := newStore(8, fixedClock())
	for i := 0; i < 5; i++ {
		s.Emit(AppEvent{Type: TypePhaseChanged})
	}
	evs, cursor := s.Since(3) // resume after seq 3 → seqs 4,5
	if len(evs) != 2 || evs[0].Seq != 4 || evs[1].Seq != 5 || cursor != 5 {
		t.Fatalf("Since(3) = %+v cursor %d; want seqs 4,5 cursor 5", seqs(evs), cursor)
	}
	// Nothing newer than the tip: empty slice, cursor still advances-aware.
	if evs, cursor := s.Since(5); len(evs) != 0 || cursor != 5 {
		t.Fatalf("Since(tip) = %d events cursor %d; want 0/5", len(evs), cursor)
	}
}

func TestRingDropsOldest(t *testing.T) {
	s := newStore(3, fixedClock()) // holds only the last 3
	for i := 0; i < 10; i++ {
		s.Emit(AppEvent{Type: TypeCreated})
	}
	evs, cursor := s.Since(0)
	if cursor != 10 {
		t.Fatalf("cursor = %d, want 10 (seq keeps counting past the ring)", cursor)
	}
	if len(evs) != 3 {
		t.Fatalf("ring held %d events, want 3", len(evs))
	}
	// The three retained are the newest (seqs 8,9,10), still in order.
	if evs[0].Seq != 8 || evs[2].Seq != 10 {
		t.Fatalf("retained seqs = %v, want 8..10", seqs(evs))
	}
}

func TestSubscribeReceivesAndDropsOnFull(t *testing.T) {
	s := newStore(16, fixedClock())
	ch, cancel := s.Subscribe(2) // tiny buffer to force a drop
	defer cancel()

	s.Emit(AppEvent{Type: TypeCreated, App: "a"})
	if got := <-ch; got.App != "a" || got.Seq != 1 {
		t.Fatalf("first delivered = %+v, want app a seq 1", got)
	}
	// Overfill without draining: buffer=2, emit 5 → excess dropped, never blocks.
	for i := 0; i < 5; i++ {
		s.Emit(AppEvent{Type: TypeCreated})
	}
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			if drained > 2 {
				t.Fatalf("drained %d, want <= 2 (drop-on-full)", drained)
			}
			// cancel closes the channel; a further receive must not panic.
			cancel()
			if _, ok := <-ch; ok {
				t.Error("channel should be closed after cancel")
			}
			return
		}
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	s := newStore(4, fixedClock())
	_, cancel := s.Subscribe(1)
	cancel()
	cancel() // must not panic / double-close
}

func seqs(evs []AppEvent) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}
