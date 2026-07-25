package trajectory

import (
	"sync"
	"testing"
)

// TestHubFanOut proves a published event reaches every current subscriber of a
// run and no subscriber of a different run.
func TestHubFanOut(t *testing.T) {
	h := newHub()
	a := h.subscribe("run1")
	b := h.subscribe("run1")
	other := h.subscribe("run2")

	h.publish("run1", StreamEvent{Type: EventSpan, StreamSeq: 1})

	for name, sub := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case ev := <-sub.ch:
			if ev.StreamSeq != 1 {
				t.Fatalf("%s got seq %d, want 1", name, ev.StreamSeq)
			}
		default:
			t.Fatalf("%s did not receive the published event", name)
		}
	}
	select {
	case ev := <-other.ch:
		t.Fatalf("run2 subscriber received a run1 event: %+v", ev)
	default:
	}
}

// TestHubUnsubscribe proves an unsubscribed subscriber receives no further
// events and the run's entry is dropped once empty.
func TestHubUnsubscribe(t *testing.T) {
	h := newHub()
	a := h.subscribe("run1")
	h.unsubscribe("run1", a)

	h.publish("run1", StreamEvent{Type: EventSpan, StreamSeq: 1})
	select {
	case ev := <-a.ch:
		t.Fatalf("unsubscribed subscriber still received %+v", ev)
	default:
	}

	h.mu.Lock()
	_, present := h.subs["run1"]
	h.mu.Unlock()
	if present {
		t.Fatal("empty run entry was not pruned from the hub")
	}
}

// TestHubLagDropsSlowSubscriber proves a subscriber that overflows its buffer is
// dropped (its Lagged channel closes) rather than blocking the publisher, so a
// slow viewer never back-pressures the ingest path.
func TestHubLagDropsSlowSubscriber(t *testing.T) {
	h := newHub()
	a := h.subscribe("run1")

	// Never drain a.ch: overflow the buffer, then one more publish trips the drop.
	for i := 0; i < streamBuffer+5; i++ {
		h.publish("run1", StreamEvent{Type: EventSpan, StreamSeq: int64(i + 1)})
	}
	select {
	case <-a.lagged:
	default:
		t.Fatal("slow subscriber was not dropped after buffer overflow")
	}
}

// TestHubConcurrentAccess exercises subscribe/publish/unsubscribe from many
// goroutines so the race detector can prove the shared subscriber set is
// accessed race-free (SPEC-0004 "Concurrency Safety" — CI runs -race).
func TestHubConcurrentAccess(t *testing.T) {
	h := newHub()
	var wg sync.WaitGroup

	// Publishers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.publish("run1", StreamEvent{Type: EventSpan, StreamSeq: int64(j)})
			}
		}()
	}
	// Subscribers churning in and out, draining as they go.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := h.subscribe("run1")
				select {
				case <-s.ch:
				default:
				}
				h.unsubscribe("run1", s)
			}
		}()
	}
	wg.Wait()
}
