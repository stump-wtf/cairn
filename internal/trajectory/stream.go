package trajectory

import "sync"

// streamBuffer is the per-subscriber channel depth. A live viewer that falls
// this far behind the append fan-out is dropped (its Lagged channel closes) so a
// slow client can never block the ingest path; the client reconnects and
// replays from its Last-Event-ID, so no span is actually lost — the buffer is a
// backpressure valve, not the durability boundary (SPEC-0004 "Concurrency
// Safety"; the span rows are the source of truth).
const streamBuffer = 256

// EventType classifies a live stream event.
type EventType string

const (
	// EventSpan carries one appended span and its stream_seq cursor.
	EventSpan EventType = "span"
	// EventStatus signals a run lifecycle transition (currently only the
	// open→closed edge), so a connected viewer flips its live badge and the SSE
	// stream ends cleanly.
	EventStatus EventType = "status"
)

// StreamEvent is one fan-out unit delivered to a live subscriber. For EventSpan,
// Span and StreamSeq are set (StreamSeq is the SSE id: and the resume cursor).
// For EventStatus, Status is set.
type StreamEvent struct {
	Type      EventType
	StreamSeq int64
	Span      *Span
	Status    Status
}

// subscriber is one live tail. ch is buffered so publish never blocks; if it
// fills, drop() closes lagged exactly once to tell the reader to tear down and
// resume via Last-Event-ID. The reader never closes ch (multiple publishers
// hold the hub lock and send into it), so there is no close-of-channel race.
type subscriber struct {
	ch     chan StreamEvent
	lagged chan struct{}
	once   sync.Once
}

func (s *subscriber) drop() { s.once.Do(func() { close(s.lagged) }) }

// hub is the race-safe span fan-out: a run public id maps to its set of live
// subscribers. Every mutation and every publish takes mu, so subscribe,
// unsubscribe, and publish are serialized and the subscriber set is never
// observed mid-mutation (SPEC-0004 "Concurrency Safety" — CI runs -race).
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: make(map[string]map[*subscriber]struct{})} }

// subscribe registers a new subscriber for a run and returns it. The caller MUST
// unsubscribe it (the Subscription.Close the SSE handler defers).
func (h *hub) subscribe(run string) *subscriber {
	s := &subscriber{ch: make(chan StreamEvent, streamBuffer), lagged: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[run] == nil {
		h.subs[run] = make(map[*subscriber]struct{})
	}
	h.subs[run][s] = struct{}{}
	return s
}

// unsubscribe removes a subscriber; after it returns no further publish will
// reach that subscriber, so the reader goroutine can stop draining and be
// reaped (SPEC-0004 "Disconnected SSE client is reaped").
func (h *hub) unsubscribe(run string, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[run]
	if m == nil {
		return
	}
	delete(m, s)
	if len(m) == 0 {
		delete(h.subs, run)
	}
}

// publish fans an event out to every current subscriber of a run. It is
// non-blocking: a subscriber whose buffer is full is dropped rather than
// stalling the ingest path, so one slow viewer cannot back-pressure appends. The
// hub lock serializes publish against subscribe/unsubscribe.
func (h *hub) publish(run string, ev StreamEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[run] {
		select {
		case s.ch <- ev:
		default:
			s.drop()
		}
	}
}

// Subscription is a live tail handle over a run's append stream, returned by
// Service.Subscribe. The caller drives Events until Lagged fires, its context is
// cancelled, or an EventStatus(closed) arrives, then calls Close.
type Subscription struct {
	hub *hub
	run string
	sub *subscriber
}

// Events is the channel of live span/status events for the run.
func (s *Subscription) Events() <-chan StreamEvent { return s.sub.ch }

// Lagged closes when this subscriber fell too far behind and was dropped; the
// reader should tear down and let the client resume from its Last-Event-ID.
func (s *Subscription) Lagged() <-chan struct{} { return s.sub.lagged }

// Close unsubscribes the tail. It is idempotent-safe to defer once.
func (s *Subscription) Close() { s.hub.unsubscribe(s.run, s.sub) }

// Subscribe registers a live tail for a run's append stream. It does not itself
// verify the run exists — the SSE handler subscribes BEFORE the replay read so
// an append landing between the replay snapshot and the tail loop is buffered,
// not lost (SPEC-0004 "Late joiner sees history then live"), then resolves the
// run (uniform 404) for the replay. The caller MUST Close the returned handle.
func (s *Service) Subscribe(run string) *Subscription {
	return &Subscription{hub: s.hub, run: run, sub: s.hub.subscribe(run)}
}
