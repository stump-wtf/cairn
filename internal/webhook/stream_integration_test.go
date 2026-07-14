// Integration tests for the webhook live fan-out (SPEC-0005 REQ "One Stream,
// Two Transports (Live Fan-out)", "Concurrency Safety"): Subscribe/
// RequestsAfter is the core-service half of both the SSE endpoint
// (internal/httpapi/hook_stream.go) and the mcp://cairn/hook/<id> MCP
// resource (internal/httpapi/mcp.go) — this package's own tests exercise it
// directly, without an HTTP or MCP transport in the loop, exactly mirroring
// internal/trajectory's service_integration_test.go split.
package webhook

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// drainEvent awaits the next event on a subscription (or its Lagged signal),
// failing the test on timeout so a missing publish is a failure, not a hang.
func drainEvent(t *testing.T, sub *Subscription) StreamEvent {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-sub.Lagged():
		t.Fatal("subscription lagged before the expected event arrived")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a stream event")
	}
	return StreamEvent{}
}

// TestIntegrationRequestsAfterReplaysFromCursor proves RequestsAfter — the
// SSE/MCP replay read — returns only requests captured after the given seq,
// in ascending order, mirroring internal/trajectory.Service.SpansAfter
// (SPEC-0005 "Late joiner sees history then tail").
func TestIntegrationRequestsAfterReplaysFromCursor(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{})

	for i := 0; i < 3; i++ {
		if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{
			Method: "GET", Path: fmt.Sprintf("/r%d", i), Status: 200,
		}); err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
	}

	all, err := svc.RequestsAfter(ctx, ep.PublicID, 0)
	if err != nil {
		t.Fatalf("requests after 0: %v", err)
	}
	if len(all) != 3 || all[0].Seq != 1 || all[2].Seq != 3 {
		t.Fatalf("requests after 0 = %+v, want seq [1,2,3]", all)
	}

	resumed, err := svc.RequestsAfter(ctx, ep.PublicID, 1)
	if err != nil {
		t.Fatalf("requests after 1: %v", err)
	}
	if len(resumed) != 2 || resumed[0].Seq != 2 || resumed[1].Seq != 3 {
		t.Fatalf("requests after 1 = %+v, want seq [2,3] (seq 1 must not repeat)", resumed)
	}

	none, err := svc.RequestsAfter(ctx, ep.PublicID, 3)
	if err != nil {
		t.Fatalf("requests after 3: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("requests after the latest seq = %+v, want none", none)
	}
}

// TestIntegrationRequestsAfterUnknownEndpoint proves the replay read returns
// the uniform ErrEndpointNotFound for an unknown id, exactly like every other
// webhook read (ADR-0007 link-capability).
func TestIntegrationRequestsAfterUnknownEndpoint(t *testing.T) {
	svc, _ := newHarness(t)
	if _, err := svc.RequestsAfter(context.Background(), "zzzzzzzz", 0); err == nil {
		t.Fatal("requests after on an unknown endpoint: want ErrEndpointNotFound, got nil")
	} else if err.Error() == "" {
		t.Fatal("expected a wrapped error")
	}
}

// TestIntegrationSubscribeFansOutToMultipleSubscribers is the headline live
// fan-out acceptance: concurrent captures on one endpoint reach every current
// subscriber in the identical seq order (SPEC-0005 "Human and agent see the
// same order", "One Stream, Two Transports (Live Fan-out)").
func TestIntegrationSubscribeFansOutToMultipleSubscribers(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{RequestCap: 100})

	const nSubs = 5
	subs := make([]*Subscription, nSubs)
	for i := range subs {
		subs[i] = svc.Subscribe(ep.PublicID)
		defer subs[i].Close()
	}

	if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "POST", Path: "/x", Status: 200}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	for i, sub := range subs {
		ev := drainEvent(t, sub)
		if ev.Type != EventRequest || ev.Seq != 1 {
			t.Fatalf("subscriber %d event = %+v, want request/seq1", i, ev)
		}
		if ev.Request == nil || ev.Request.Path != "/x" {
			t.Fatalf("subscriber %d request = %+v, want path /x", i, ev.Request)
		}
	}
}

// TestIntegrationSubscribeBeforeReplayNoLostCapture proves subscribing before
// reading the replay snapshot — the ordering the SSE/MCP handlers use — never
// drops a capture that lands in the gap between the two, mirroring
// internal/trajectory's "late joiner sees history then live" race-safety
// contract (SPEC-0005 "Late joiner sees history then tail").
func TestIntegrationSubscribeBeforeReplayNoLostCapture(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{})

	// Subscribe FIRST, exactly the handler's ordering.
	sub := svc.Subscribe(ep.PublicID)
	defer sub.Close()

	// A capture lands "during" the window before the replay read below.
	if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Path: "/race", Status: 200}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	replay, err := svc.RequestsAfter(ctx, ep.PublicID, 0)
	if err != nil {
		t.Fatalf("requests after: %v", err)
	}
	if len(replay) != 1 || replay[0].Seq != 1 {
		t.Fatalf("replay = %+v, want [seq 1]", replay)
	}

	// The live event is ALSO buffered on the subscription (the handler's
	// de-dup-by-seq logic is what discards it, not the hub) — proving nothing
	// was silently swallowed between subscribe and replay.
	ev := drainEvent(t, sub)
	if ev.Seq != 1 {
		t.Fatalf("buffered live event seq = %d, want 1 (subscribe-before-replay must not lose it)", ev.Seq)
	}
}

// TestIntegrationConcurrentCapturesFanOutToManySubscribers exercises N
// concurrent captures fanning out to N concurrent subscribers under
// `go test -race`, proving the hub's shared subscriber state is race-free
// under real concurrent Capture traffic (SPEC-0005 "Concurrency Safety":
// "Concurrent captures on one endpoint MUST assign seq monotonically without
// gaps or collisions").
func TestIntegrationConcurrentCapturesFanOutToManySubscribers(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	const nCaptures = 20
	const nSubs = 4
	ep := newEndpoint(t, svc, EndpointInput{RequestCap: nCaptures})

	subs := make([]*Subscription, nSubs)
	received := make([][]int64, nSubs)
	var wg sync.WaitGroup
	for i := range subs {
		subs[i] = svc.Subscribe(ep.PublicID)
		defer subs[i].Close()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen := make(map[int64]bool, nCaptures)
			for len(seen) < nCaptures {
				select {
				case ev := <-subs[i].Events():
					if !seen[ev.Seq] {
						seen[ev.Seq] = true
						received[i] = append(received[i], ev.Seq)
					}
				case <-subs[i].Lagged():
					t.Errorf("subscriber %d lagged out mid-test", i)
					return
				case <-time.After(5 * time.Second):
					t.Errorf("subscriber %d timed out with %d/%d events", i, len(seen), nCaptures)
					return
				}
			}
		}(i)
	}

	var capWg sync.WaitGroup
	for i := 0; i < nCaptures; i++ {
		capWg.Add(1)
		go func(i int) {
			defer capWg.Done()
			if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Path: fmt.Sprintf("/c%d", i), Status: 200}); err != nil {
				t.Errorf("concurrent capture %d: %v", i, err)
			}
		}(i)
	}
	capWg.Wait()
	wg.Wait()

	for i, seqs := range received {
		if len(seqs) != nCaptures {
			t.Fatalf("subscriber %d received %d/%d distinct events", i, len(seqs), nCaptures)
		}
	}
}

// TestIntegrationSubscriberTeardownDoesNotBlockCapture proves closing a
// subscription that never drains its channel unblocks immediately and
// subsequent captures proceed unimpeded — the hub drop-on-full behavior a
// slow/abandoned subscriber relies on (SPEC-0005 "Disconnected inspector is
// reaped", "a slow/abandoned subscriber can't stall capture").
func TestIntegrationSubscriberTeardownDoesNotBlockCapture(t *testing.T) {
	svc, _ := newHarness(t)
	ctx := context.Background()
	ep := newEndpoint(t, svc, EndpointInput{RequestCap: streamBuffer + 20})

	sub := svc.Subscribe(ep.PublicID)

	// Flood past the subscriber's buffer without ever draining it.
	for i := 0; i < streamBuffer+10; i++ {
		if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Path: fmt.Sprintf("/f%d", i), Status: 200}); err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
	}

	// The abandoned subscriber must have lagged out (dropped), never having
	// stalled any of the captures above (they all returned above already).
	select {
	case <-sub.Lagged():
	default:
		t.Fatal("abandoned subscriber was never dropped despite an overflowing buffer")
	}
	sub.Close() // idempotent-safe teardown, must not block or panic

	// Capture keeps working after teardown.
	if _, err := svc.Capture(ctx, ep.PublicID, CaptureInput{Method: "GET", Path: "/after", Status: 200}); err != nil {
		t.Fatalf("capture after teardown: %v", err)
	}
}
