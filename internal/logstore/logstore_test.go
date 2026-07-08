package logstore

import (
	"fmt"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func rec(text string) Record {
	return Record{TimeMs: 1, Source: SourceService, Stream: StreamStdout, Text: text}
}

func TestAppendAndTailRead(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []string{"one", "two", "three"} {
		if err := s.Append("sbx_a", rec(m)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	recs, next, err := s.Read("sbx_a", -1, 1<<20, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 3 || recs[0].Text != "one" || recs[2].Text != "three" {
		t.Fatalf("tail read = %+v", recs)
	}
	if next <= 0 {
		t.Errorf("next offset = %d, want > 0", next)
	}
}

func TestFollowFromOffset(t *testing.T) {
	s := newTestStore(t)
	_ = s.Append("sbx_a", rec("first"))
	_, next, err := s.Read("sbx_a", -1, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing new yet.
	recs, next2, _ := s.Read("sbx_a", next, 0, 0)
	if len(recs) != 0 || next2 != next {
		t.Fatalf("empty follow = %d recs, next %d (was %d)", len(recs), next2, next)
	}
	// Append and follow returns only the new record.
	_ = s.Append("sbx_a", rec("second"))
	recs, _, _ = s.Read("sbx_a", next, 0, 0)
	if len(recs) != 1 || recs[0].Text != "second" {
		t.Fatalf("follow after append = %+v", recs)
	}
}

func TestReadMaxCap(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 10; i++ {
		_ = s.Append("sbx_a", rec(fmt.Sprintf("line-%d", i)))
	}
	recs, next, _ := s.Read("sbx_a", 0, 0, 3)
	if len(recs) != 3 || recs[0].Text != "line-0" {
		t.Fatalf("max-capped read = %+v", recs)
	}
	// Resuming from next continues past the cap.
	more, _, _ := s.Read("sbx_a", next, 0, 3)
	if len(more) != 3 || more[0].Text != "line-3" {
		t.Fatalf("resume = %+v", more)
	}
}

func TestMissingLogIsEmpty(t *testing.T) {
	s := newTestStore(t)
	recs, next, err := s.Read("sbx_none", -1, 1<<20, 0)
	if err != nil || recs != nil || next != 0 {
		t.Fatalf("missing log = (%v, %d, %v), want (nil, 0, nil)", recs, next, err)
	}
}

func TestSizeCapTruncates(t *testing.T) {
	s := newTestStore(t)
	s.maxFileBytes = 200 // tiny cap
	for i := 0; i < 50; i++ {
		if err := s.Append("sbx_a", rec(fmt.Sprintf("chatty-line-%d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	recs, _, err := s.Read("sbx_a", -1, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no records after cap")
	}
	last := recs[len(recs)-1]
	if last.Stream != StreamEvent || last.Text == "" {
		t.Errorf("expected a truncation event marker last, got %+v", last)
	}
	// Further appends are dropped (still truncated).
	before := len(recs)
	_ = s.Append("sbx_a", rec("after-truncation"))
	recs2, _, _ := s.Read("sbx_a", -1, 1<<20, 0)
	if len(recs2) != before {
		t.Errorf("append after truncation grew log: %d → %d", before, len(recs2))
	}
}

func TestInvalidIDRejected(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{"", "../escape", "a/b", "."} {
		if err := s.Append(bad, rec("x")); err == nil {
			t.Errorf("Append(%q) = nil, want error", bad)
		}
		if _, _, err := s.Read(bad, -1, 1<<20, 0); err == nil {
			t.Errorf("Read(%q) = nil, want error", bad)
		}
	}
}

func TestConcurrentAppends(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = s.Append("sbx_a", rec(fmt.Sprintf("g%d-%d", g, i)))
			}
		}(g)
	}
	wg.Wait()
	recs, _, err := s.Read("sbx_a", -1, 1<<30, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 8*50 {
		t.Fatalf("got %d records, want %d", len(recs), 8*50)
	}
}
