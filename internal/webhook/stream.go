package webhook

import "sync"

// streamBuffer is the per-subscriber channel depth. A live inspector or MCP
// reader that falls this far behind the capture fan-out is dropped (its
// Lagged channel closes) so a slow client can never block the anonymous
// ingress path — the client reconnects and replays from its Last-Event-ID
// (the request's own `seq`, already the stable per-endpoint stream cursor),
// so no capture is actually lost — the buffer is a backpressure valve, not
// the durability boundary, exactly mirroring internal/trajectory's hub
// (SPEC-0005 "Concurrency Safety": "a slow/abandoned subscriber can't stall
// capture").
const streamBuffer = 256

// EventType classifies a live stream event. Unlike internal/trajectory's hub,
// a webhook endpoint has no open/closed lifecycle transition to signal — it
// stays live until its artifact TTL expires, at which point captures and
// reads alike fall back to the uniform not-found (SPEC-0005 "Expired endpoint
// stops capturing") — so EventRequest is the only event this hub ever
// publishes.
type EventType string

// EventRequest carries one freshly captured request and its seq (the SSE id:
// and MCP resume cursor).
const EventRequest EventType = "request"

// StreamEvent is one fan-out unit delivered to a live subscriber: the
// captured Request and its Seq (SPEC-0005 "One Stream, Two Transports").
type StreamEvent struct {
	Type    EventType
	Seq     int64
	Request *Request
}

// subscriber is one live tail. ch is buffered so publish never blocks; if it
// fills, drop() closes lagged exactly once to tell the reader to tear down
// and resume via Last-Event-ID. The reader never closes ch (multiple
// publishers hold the hub lock and send into it), so there is no
// close-of-channel race — verbatim internal/trajectory's subscriber.
type subscriber struct {
	ch     chan StreamEvent
	lagged chan struct{}
	once   sync.Once
}

func (s *subscriber) drop() { s.once.Do(func() { close(s.lagged) }) }

// hub is the race-safe request fan-out: an endpoint public id maps to its set
// of live subscribers. Every mutation and every publish takes mu, so
// subscribe, unsubscribe, and publish are serialized and the subscriber set
// is never observed mid-mutation (SPEC-0005 "Concurrency Safety" — CI runs
// -race).
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: make(map[string]map[*subscriber]struct{})} }

// subscribe registers a new subscriber for an endpoint and returns it. The
// caller MUST unsubscribe it (the Subscription.Close the SSE/MCP handler
// defers).
func (h *hub) subscribe(endpoint string) *subscriber {
	s := &subscriber{ch: make(chan StreamEvent, streamBuffer), lagged: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[endpoint] == nil {
		h.subs[endpoint] = make(map[*subscriber]struct{})
	}
	h.subs[endpoint][s] = struct{}{}
	return s
}

// unsubscribe removes a subscriber; after it returns no further publish will
// reach that subscriber, so the reader goroutine can stop draining and be
// reaped (SPEC-0005 "Disconnected inspector is reaped").
func (h *hub) unsubscribe(endpoint string, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[endpoint]
	if m == nil {
		return
	}
	delete(m, s)
	if len(m) == 0 {
		delete(h.subs, endpoint)
	}
}

// publish fans a captured request out to every current subscriber of an
// endpoint. It is non-blocking: a subscriber whose buffer is full is dropped
// rather than stalling the capture path, so one slow viewer or agent can
// never back-pressure an inbound capture (SPEC-0005 "a slow/abandoned
// subscriber can't stall capture"). The hub lock serializes publish against
// subscribe/unsubscribe.
func (h *hub) publish(endpoint string, ev StreamEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[endpoint] {
		select {
		case s.ch <- ev:
		default:
			s.drop()
		}
	}
}

// Subscription is a live tail handle over an endpoint's capture stream,
// returned by Service.Subscribe. The caller drives Events until Lagged
// fires or its context is cancelled, then calls Close.
type Subscription struct {
	hub      *hub
	endpoint string
	sub      *subscriber
}

// Events is the channel of live captured-request events for the endpoint.
func (s *Subscription) Events() <-chan StreamEvent { return s.sub.ch }

// Lagged closes when this subscriber fell too far behind and was dropped; the
// reader should tear down and let the client resume from its Last-Event-ID.
func (s *Subscription) Lagged() <-chan struct{} { return s.sub.lagged }

// Close unsubscribes the tail. It is safe to call from a deferred call site.
func (s *Subscription) Close() { s.hub.unsubscribe(s.endpoint, s.sub) }

// Subscribe registers a live tail for an endpoint's capture stream. It does
// not itself verify the endpoint exists — the SSE/MCP handler subscribes
// BEFORE the replay read so a capture landing between the replay snapshot and
// the tail loop is buffered, not lost (SPEC-0005 "Late joiner sees history
// then tail"), then resolves the endpoint (uniform not-found) for the replay.
// The caller MUST Close the returned handle.
func (s *Service) Subscribe(endpoint string) *Subscription {
	return &Subscription{hub: s.hub, endpoint: endpoint, sub: s.hub.subscribe(endpoint)}
}
