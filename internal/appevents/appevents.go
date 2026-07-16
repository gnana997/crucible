// Package appevents is a small in-memory stream of app lifecycle events —
// created, phase changes (booted/slept/woke/crashed/…), health flips, domain
// changes, delete. A control plane consumes it for an activity timeline and to
// compute exact awake-intervals for usage accounting (the exact sleep/wake timestamps a
// 60s usage poll can't give). It is deliberately ephemeral: a bounded ring plus
// best-effort fanout to live subscribers (drop-on-full, so a slow consumer can
// never back-pressure the app), mirroring internal/logstore. The daemon exposes
// it over GET /events; the control plane persists what it consumes.
package appevents

import (
	"sync"
	"time"
)

// Event types.
const (
	TypeCreated       = "created"
	TypeUpdated       = "updated"
	TypeDeleted       = "deleted"
	TypeDomainAdded   = "domain_added"
	TypeDomainRemoved = "domain_removed"
	TypePhaseChanged  = "phase_changed" // the workhorse; Attrs carry from/to (+ wake_latency_ms)
	TypeHealthChanged = "health_changed"
	TypeRollout       = "rollout"
)

// AppEvent is one app lifecycle transition. Seq is the monotonic cursor a reader
// resumes from (GET /events?since=<seq>).
type AppEvent struct {
	Seq      uint64         `json:"seq"`
	Time     time.Time      `json:"time"`
	App      string         `json:"app"`
	AppID    string         `json:"app_id"`
	Instance string         `json:"instance,omitempty"`
	Type     string         `json:"type"`
	Reason   string         `json:"reason,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
}

// DefaultBuffer is the ring size when New is given a non-positive size.
const DefaultBuffer = 1024

// Store is a bounded ring of recent events plus a subscriber fanout.
type Store struct {
	now  func() time.Time
	size int

	mu  sync.Mutex
	buf []AppEvent // chronological (ascending Seq); at most size entries
	seq uint64

	subMu   sync.Mutex
	subs    map[int]chan AppEvent
	nextSub int
}

// New returns a store holding the last `size` events (DefaultBuffer if <= 0).
func New(size int) *Store { return newStore(size, time.Now) }

func newStore(size int, now func() time.Time) *Store {
	if size <= 0 {
		size = DefaultBuffer
	}
	return &Store{now: now, size: size, subs: map[int]chan AppEvent{}}
}

// Emit stamps the event with the next Seq + a timestamp, appends it to the ring
// (dropping the oldest past capacity), and fans it out to subscribers.
func (s *Store) Emit(e AppEvent) {
	s.mu.Lock()
	s.seq++
	e.Seq = s.seq
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	s.buf = append(s.buf, e)
	if len(s.buf) > s.size {
		// Keep the newest `size`; copy back so the backing array stays bounded.
		copy(s.buf, s.buf[len(s.buf)-s.size:])
		s.buf = s.buf[:s.size]
	}
	s.mu.Unlock()
	s.fanout(e)
}

// Since returns the events still in the ring with Seq > after, in order, plus
// the current max Seq (so a reader can resume even when nothing is returned).
func (s *Store) Since(after uint64) (events []AppEvent, cursor uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.buf {
		if e.Seq > after {
			events = append(events, e)
		}
	}
	return events, s.seq
}

// Subscribe returns a channel of future events and a cancel func. Delivery is
// drop-on-full (buffer defaults to DefaultBuffer) so a slow consumer never
// blocks Emit. The channel is closed by cancel.
func (s *Store) Subscribe(buffer int) (<-chan AppEvent, func()) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	ch := make(chan AppEvent, buffer)
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	s.subMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.subMu.Lock()
			delete(s.subs, id)
			s.subMu.Unlock()
			close(ch)
		})
	}
}

// fanout delivers e to every live subscriber without blocking. A subscriber is
// removed from the map (under subMu) before its channel is closed by cancel, so
// there is no send-on-closed-channel race.
func (s *Store) fanout(e AppEvent) {
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
	s.subMu.Unlock()
}
