package webhook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	mu    sync.Mutex
	subs  map[string]map[*subscriber]struct{}
	order map[string]*orderState
}

func newHub() *hub {
	return &hub{
		subs:  make(map[string]map[*subscriber]struct{}),
		order: make(map[string]*orderState),
	}
}

// orderState is one endpoint's fan-out ordering window. Captures assign seq
// and commit their rows under the endpoint's row lock, but the publish that
// follows commit is unsynchronized, so two captures can reach the hub with
// their seqs inverted; the SSE/MCP tail then de-duplicates by seq and would
// permanently drop the late, lower-seq event for every subscriber. next is
// the next seq this endpoint's fan-out will deliver (committed captures
// consume gapless seq numbers, so next only ever advances by one), and
// pending holds events that arrived ahead of it, waiting for the gap to
// close.
type orderState struct {
	next    int64
	pending map[int64]StreamEvent
}

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
// reaped (SPEC-0005 "Disconnected inspector is reaped"). Pruning the
// endpoint's subscriber entry also prunes its order state: with no live tail
// there is nothing to keep ordered, and the rows are durable — the next
// subscriber replays them from the ring buffer and re-anchors via seed.
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
		delete(h.order, endpoint)
	}
}

// seed anchors an endpoint's ordering window at seq+1. Service.Subscribe
// calls it with the endpoint's committed next_seq (the row counter every
// Capture bumps under the row lock) before the SSE/MCP handler reads its
// replay snapshot: every seq <= that counter is already committed, so a
// subscriber's replay covers it, and every capture still to publish carries
// a higher, gapless seq. Best-effort and idempotent — an endpoint whose
// order state already exists (an earlier seed, or a publish that raced the
// seed) is left alone; its window is either equally or more advanced, which
// at worst replays a duplicate the tail's seq de-duplication already skips.
func (h *hub) seed(endpoint string, seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.order[endpoint] == nil {
		h.order[endpoint] = &orderState{next: seq + 1, pending: make(map[int64]StreamEvent)}
	}
}

// publish fans a captured request out to every current subscriber of an
// endpoint, in seq order. It is non-blocking: a subscriber whose buffer is
// full is dropped rather than stalling the capture path, so one slow viewer
// or agent can never back-pressure an inbound capture (SPEC-0005 "a
// slow/abandoned subscriber can't stall capture"). The hub lock serializes
// publish against subscribe/unsubscribe.
//
// Ordering: an event whose seq is above the window's next is buffered in
// pending until the gap closes (the capture holding the missing seq is
// committed and on its way to publish), then released in order with the rest
// of the gap. An event at or below next is a duplicate the subscriber's
// replay already covered, so it is dropped rather than re-delivered.
//
// With no subscribers there is nothing to fan out or keep ordered — the
// capture is durable and the next subscriber replays it from the ring buffer
// — so publish prunes the endpoint's order state instead of accumulating
// window state that nothing else would ever reap.
func (h *hub) publish(endpoint string, ev StreamEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subs[endpoint]
	if len(subs) == 0 {
		delete(h.order, endpoint)
		return
	}
	o := h.order[endpoint]
	if o == nil {
		// First publish since the last prune: the seq counter may be well
		// past one, so the window cannot know whether a lower committed seq
		// is still in its commit→publish gap — anchor here and accept that
		// anything older is the replay's to cover. The subscribe-time seed
		// normally wins this race and anchors below the committed seq
		// instead, so the anchor path only fires when the seed was skipped.
		o = &orderState{next: ev.Seq, pending: make(map[int64]StreamEvent)}
		h.order[endpoint] = o
	} else if ev.Seq < o.next {
		return // straggler below the delivered window: replay's territory
	} else if ev.Seq > o.next {
		o.pending[ev.Seq] = ev
		return
	}
	h.fanoutLocked(subs, ev)
	o.next++
	for {
		buffered, ok := o.pending[o.next]
		if !ok {
			break
		}
		delete(o.pending, o.next)
		h.fanoutLocked(subs, buffered)
		o.next++
	}
}

// fanoutLocked delivers one event to every current subscriber of an
// endpoint. The caller holds h.mu and the endpoint's order window is
// already advanced past ev, so delivery order is publish order.
func (h *hub) fanoutLocked(subs map[*subscriber]struct{}, ev StreamEvent) {
	for s := range subs {
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
//
// Before returning it also anchors the endpoint's fan-out ordering window at
// the committed seq counter, so every capture that publishes from here on is
// delivered strictly in seq order (see hub.publish). The anchor read is
// best-effort and never fails the subscription: an unknown endpoint or a
// transient read error simply leaves the window to anchor on the first
// publish, which the handler's uniform not-found already covers.
func (s *Service) Subscribe(endpoint string) *Subscription {
	sub := &Subscription{hub: s.hub, endpoint: endpoint, sub: s.hub.subscribe(endpoint)}
	// Bounded so a wedged pool can never hang the subscription itself; a
	// cancelled or failed anchor read just leaves the window to anchor on the
	// first publish.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if seq, err := s.committedSeq(ctx, endpoint); err == nil {
		s.hub.seed(endpoint, seq)
	}
	return sub
}

// committedSeq reads an endpoint's committed next_seq counter — the value the
// row lock's UPDATE bumps per Capture, so every capture still to publish
// carries a higher seq and every seq at or below it is already committed and
// covered by a subsequent replay read. ErrEndpointNotFound (unknown or
// expired endpoint) is reported like every other webhook read (ADR-0007).
func (s *Service) committedSeq(ctx context.Context, publicID string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		`SELECT h.next_seq FROM hooks h JOIN artifacts a ON a.id = h.artifact_id
		 WHERE a.public_id = $1 AND a.expires_at > now()`, publicID,
	).Scan(&seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("webhook: endpoint %s: %w", publicID, ErrEndpointNotFound)
		}
		return 0, fmt.Errorf("webhook: resolve endpoint %s: %w", publicID, err)
	}
	return seq, nil
}
