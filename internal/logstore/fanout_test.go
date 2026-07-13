package logstore

import (
	"testing"
	"time"
)

func TestFanoutDelivers(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch, unsub := s.Subscribe(8)
	defer unsub()

	if err := s.Append("sbx_a", Record{TimeMs: 1, Source: SourceService, Stream: StreamStdout, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.ID != "sbx_a" || ev.Rec.Text != "hello" {
			t.Errorf("got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered")
	}
}

func TestFanoutDropsWhenFull(t *testing.T) {
	s, _ := New(t.TempDir())
	ch, unsub := s.Subscribe(2) // tiny buffer, no reader draining
	defer unsub()

	for i := 0; i < 20; i++ { // must not block despite the full buffer
		_ = s.Append("sbx_a", Record{Text: "x"})
	}
	drained := 0
	for draining := true; draining; {
		select {
		case <-ch:
			drained++
		default:
			draining = false
		}
	}
	if drained != 2 {
		t.Errorf("drained %d, want 2 (buffer size; the rest dropped)", drained)
	}
}

func TestFanoutUnsubscribeClosesChannel(t *testing.T) {
	s, _ := New(t.TempDir())
	ch, unsub := s.Subscribe(4)
	unsub()
	if _, ok := <-ch; ok {
		t.Error("channel not closed after unsubscribe")
	}
	// an append after unsubscribe must not panic (send-on-closed guard)
	if err := s.Append("sbx_a", Record{Text: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestFanoutCloseClosesSubs(t *testing.T) {
	s, _ := New(t.TempDir())
	ch, _ := s.Subscribe(4)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Error("channel not closed after store Close")
	}
}
