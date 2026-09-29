package webhook

import (
	"sync"
	"testing"
)

// TestHubFanOut proves a published event reaches every current subscriber of
// an endpoint and no subscriber of a different endpoint.
func TestHubFanOut(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")
	b := h.subscribe("hook1")
	other := h.subscribe("hook2")

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 1})

	for name, sub := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case ev := <-sub.ch:
			if ev.Seq != 1 {
				t.Fatalf("%s got seq %d, want 1", name, ev.Seq)
			}
		default:
			t.Fatalf("%s did not receive the published event", name)
		}
	}
	select {
	case ev := <-other.ch:
		t.Fatalf("hook2 subscriber received a hook1 event: %+v", ev)
	default:
	}
}

// TestHubUnsubscribe proves an unsubscribed subscriber receives no further
// events and the endpoint's entry is dropped once empty.
func TestHubUnsubscribe(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")
	h.unsubscribe("hook1", a)

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 1})
	select {
	case ev := <-a.ch:
		t.Fatalf("unsubscribed subscriber still received %+v", ev)
	default:
	}

	h.mu.Lock()
	_, present := h.subs["hook1"]
	h.mu.Unlock()
	if present {
		t.Fatal("empty endpoint entry was not pruned from the hub")
	}
}

// TestHubLagDropsSlowSubscriber proves a subscriber that overflows its buffer
// is dropped (its Lagged channel closes) rather than blocking the publisher,
// so a slow viewer or agent can never back-pressure capture (SPEC-0005 "a
// slow/abandoned subscriber can't stall capture").
func TestHubLagDropsSlowSubscriber(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")

	// Never drain a.ch: overflow the buffer, then one more publish trips the drop.
	for i := 0; i < streamBuffer+5; i++ {
		h.publish("hook1", StreamEvent{Type: EventRequest, Seq: int64(i + 1)})
	}
	select {
	case <-a.lagged:
	default:
		t.Fatal("slow subscriber was not dropped after buffer overflow")
	}
}

// TestHubConcurrentAccess exercises subscribe/publish/unsubscribe from many
// goroutines so the race detector can prove the shared subscriber set is
// accessed race-free (SPEC-0005 "Concurrency Safety" — CI runs -race).
func TestHubConcurrentAccess(t *testing.T) {
	h := newHub()
	var wg sync.WaitGroup

	// Publishers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.publish("hook1", StreamEvent{Type: EventRequest, Seq: int64(j)})
			}
		}()
	}
	// Subscribers churning in and out, draining as they go.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := h.subscribe("hook1")
				select {
				case <-s.ch:
				default:
				}
				h.unsubscribe("hook1", s)
			}
		}()
	}
	wg.Wait()
}

// TestHubPublishReordersInvertedSeqs is the regression test for the fan-out
// ordering race: concurrent captures serialize their seq assignment and row
// writes under the endpoint's row lock, but the fan-out publish happens after
// commit, so two captures can reach the hub with their seqs inverted — exactly
// what this test replays by publishing seq 2 before seq 1 after seeding the
// window at the committed seq, the way Service.Subscribe anchors it. The hub must hand
// the subscriber 1 then 2: without per-stream reordering the tail's
// de-dup-by-seq skip (internal/httpapi/hook_stream.go) would permanently drop
// the late, lower-seq event for every subscriber — the "24/25 delivered, then
// every subscriber times out" CI flake (SPEC-0005 "Human and agent see the
// same order").
func TestHubPublishReordersInvertedSeqs(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")
	h.seed("hook1", 0)

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 2})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 1})

	var got []int64
	for i := 0; i < 2; i++ {
		select {
		case ev := <-a.ch:
			got = append(got, ev.Seq)
		default:
			t.Fatalf("subscriber received %d events, want 2", len(got))
		}
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("subscriber saw seqs %v, want [1 2] (late lower-seq event dropped)", got)
	}
}

// TestHubPublishDrainsPendingInOrder proves a gap buffers rather than drops:
// seq 3 arrives while 2 is still in the commit→publish gap, then 2 lands and
// releases it, and a duplicate 2 arriving last is skipped as a straggler.
func TestHubPublishDrainsPendingInOrder(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")
	h.seed("hook1", 0)

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 1})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 3})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 2})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 2})

	want := []int64{1, 2, 3}
	for i, w := range want {
		select {
		case ev := <-a.ch:
			if ev.Seq != w {
				t.Fatalf("event %d = seq %d, want %d", i, ev.Seq, w)
			}
		default:
			t.Fatalf("event %d missing, want seq %d", i, w)
		}
	}
	select {
	case ev := <-a.ch:
		t.Fatalf("subscriber received a fourth event %+v, want none (straggler must be dropped)", ev)
	default:
	}
}

// TestHubSeedAnchorsOrderedWindow proves the subscribe-time seed anchors the
// ordered window at the endpoint's committed seq: an event the replay already
// covered (seq <= the seed) is dropped as a duplicate, and the gapless live
// stream continues from seed+1 in order (SPEC-0005 "Late joiner sees history
// then tail").
func TestHubSeedAnchorsOrderedWindow(t *testing.T) {
	h := newHub()
	a := h.subscribe("hook1")
	h.seed("hook1", 4)

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 4})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 6})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 5})

	want := []int64{5, 6}
	for i, w := range want {
		select {
		case ev := <-a.ch:
			if ev.Seq != w {
				t.Fatalf("event %d = seq %d, want %d", i, ev.Seq, w)
			}
		default:
			t.Fatalf("event %d missing, want seq %d", i, w)
		}
	}
	select {
	case ev := <-a.ch:
		t.Fatalf("subscriber received a third event %+v, want none (seq 4 is replay-covered)", ev)
	default:
	}
}

// TestHubOrderStatePruned proves the per-stream order-tracking state does not
// outlive the fan-out: a publish with zero subscribers leaves none behind, and
// the state created while a subscriber was attached is pruned when the last
// subscriber of the endpoint unsubscribes.
func TestHubOrderStatePruned(t *testing.T) {
	h := newHub()

	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 5})
	h.mu.Lock()
	_, ordered := h.order["hook1"]
	h.mu.Unlock()
	if ordered {
		t.Fatal("publish with zero subscribers left order state behind")
	}

	a := h.subscribe("hook1")
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 6})
	h.publish("hook1", StreamEvent{Type: EventRequest, Seq: 8})
	h.mu.Lock()
	_, ordered = h.order["hook1"]
	h.mu.Unlock()
	if !ordered {
		t.Fatal("order state missing while a subscriber is attached and a gap is pending")
	}

	h.unsubscribe("hook1", a)
	h.mu.Lock()
	_, ordered = h.order["hook1"]
	h.mu.Unlock()
	if ordered {
		t.Fatal("order state survived the last subscriber's unsubscribe")
	}
}
