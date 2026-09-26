package outboundhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/subscription"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012; SPEC-0023 REQ "Owned
// Outbound Subscriptions", REQ "Events Go Only to the Artifact's Workspace",
// REQ "Subscription Target Safety"

const (
	userU = "11111111-1111-4111-8111-111111111111"
	userV = "22222222-2222-4222-8222-222222222222"
	teamT = "33333333-3333-4333-8333-333333333333"
)

// fakeSub is one subscription in fakeSubs.
type fakeSub struct {
	owner  subscription.Owner
	target subscription.Target
	kinds  []string
	tags   []string
}

// fakeSubs is an in-memory Subscriptions: it applies the owner and filter
// rules the real service applies in SQL, and records every Record call.
type fakeSubs struct {
	mu      sync.Mutex
	subs    []fakeSub
	lookups []subscription.Owner
	results map[string][]subscription.Result
}

func (f *fakeSubs) add(owner subscription.Owner, id, url, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fakeSub{owner: owner, target: subscription.Target{ID: id, URL: url, Secret: []byte(secret)}})
}

func (f *fakeSubs) Targets(_ context.Context, owner subscription.Owner, m subscription.Match) ([]subscription.Target, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, owner)
	var out []subscription.Target
	for _, s := range f.subs {
		if s.owner != owner {
			continue
		}
		if len(s.kinds) > 0 && !slices.Contains(s.kinds, m.Kind) {
			continue
		}
		if len(s.tags) > 0 && !slices.ContainsFunc(s.tags, func(t string) bool { return slices.Contains(m.Tags, t) }) {
			continue
		}
		out = append(out, s.target)
	}
	return out, 0, nil
}

func (f *fakeSubs) Record(_ context.Context, id string, r subscription.Result) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results == nil {
		f.results = map[string][]subscription.Result{}
	}
	f.results[id] = append(f.results[id], r)
	return false, nil
}

func (f *fakeSubs) recorded(id string) []subscription.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]subscription.Result(nil), f.results[id]...)
}

// loopbackPolicy delivers to httptest servers: http and loopback admitted.
func loopbackPolicy() *subscription.Policy {
	return &subscription.Policy{AllowHTTP: true, PermitAddr: func(a netip.Addr) bool { return a.IsLoopback() }}
}

func testEvent() store.CreationEvent {
	return store.CreationEvent{
		PublicID:    "abc123",
		ShareType:   artifact.TypeFile,
		Title:       "hello",
		WebPath:     "/abc123",
		ActorID:     "joestump",
		Channel:     "api",
		ExpiresAt:   time.Now().Add(24 * time.Hour).UTC(),
		OwnerUserID: userU,
	}
}

// newEmitter builds an emitter over subs with backoff disabled.
func newEmitter(t *testing.T, subs *fakeSubs) *Emitter {
	t.Helper()
	old := backoffFor
	backoffFor = func(int) time.Duration { return 0 }
	t.Cleanup(func() { backoffFor = old })
	return New(subs, loopbackPolicy(), "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A nil *Emitter must be safe to call.
//
// store.Options.Emitter is an interface field, so a nil *Emitter assigned into
// it becomes a NON-nil interface wrapping a nil pointer — store's
// `s.emitter == nil` guard reads false and dispatches to this method anyway.
// Before cairn#201 that panicked on every artifact create, after the commit, so
// the artifact was written and the client was told the request failed.
//
// This asserts the receiver guard directly. The companion test lives at the
// wiring seam in cmd/cairnd, because a test that builds its own emitter cannot
// catch a bug in how main() builds one — which is why the whole suite stayed
// green while the binary panicked.
func TestEmitArtifactCreatedOnNilReceiverDoesNotPanic(t *testing.T) {
	var e *Emitter
	e.EmitArtifactCreated(testEvent()) // must not panic
}

// run starts the emitter's workers until the test ends.
func run(t *testing.T, e *Emitter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// capture is a receiver that records every delivery.
type capture struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	status int
}

func newCapture(t *testing.T, status int) *capture {
	t.Helper()
	c := &capture{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.hdrs = append(c.hdrs, r.Header.Clone())
		c.mu.Unlock()
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capture) first() ([]byte, http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[0], c.hdrs[0]
}

func TestDeliveryPayloadHeadersAndPerSubscriptionSignature(t *testing.T) {
	recv := newCapture(t, http.StatusAccepted)
	subs := &fakeSubs{}
	const secret = "a-secret-the-subscription-alone-holds"
	subs.add(subscription.Owner{UserID: userU}, "sub-1", recv.srv.URL, secret)
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a delivery", func() bool { return recv.count() == 1 })

	body, hdr := recv.first()
	var got eventBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got.Source != "cairn" || got.Kind != "artifact.created" || got.EventID == "" {
		t.Fatalf("bad envelope: %+v", got)
	}
	if got.Data.ID != "abc123" || got.Data.ShareType != "file" {
		t.Fatalf("bad data: %+v", got.Data)
	}
	if want := "https://cairn.example/abc123"; got.Data.URL != want {
		t.Fatalf("url = %q want %q", got.Data.URL, want)
	}
	if got.Data.ExpiresAt == nil {
		t.Fatal("expires_at missing")
	}
	if got := hdr.Get("X-Cairn-Event"); got != "artifact.created" {
		t.Fatalf("X-Cairn-Event = %q", got)
	}
	if evID := hdr.Get("X-Cairn-Event-Id"); evID != got.EventID {
		t.Fatalf("X-Cairn-Event-Id = %q want %q", evID, got.EventID)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if got, want := hdr.Get("X-Cairn-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("signature mismatch: got %q want %q", got, want)
	}
	// The owner selects targets and never appears on the wire (SPEC-0016).
	if bytes.Contains(body, []byte(userU)) || bytes.Contains(body, []byte("owner")) {
		t.Fatalf("the owner leaked into the payload: %s", body)
	}
	waitFor(t, "a recorded success", func() bool { return len(subs.recorded("sub-1")) == 1 })
	if r := subs.recorded("sub-1")[0]; !r.OK || r.Status != http.StatusAccepted {
		t.Fatalf("recorded %+v, want OK 202", r)
	}
}

// ownedHandoff is handoffEvent about an artifact owner owns.
func ownedHandoff(owner string) store.CreationEvent {
	ev := handoffEvent()
	ev.OwnerUserID = owner
	return ev
}

// TestEachSubscriptionSignsWithItsOwnSecret: two subscriptions of one owner
// each receive the event signed with their own secret, never a shared one.
func TestEachSubscriptionSignsWithItsOwnSecret(t *testing.T) {
	a, b := newCapture(t, 200), newCapture(t, 200)
	subs := &fakeSubs{}
	owner := subscription.Owner{UserID: userU}
	subs.add(owner, "sub-a", a.srv.URL, "secret-a-secret-a-secret-a-secret-a")
	subs.add(owner, "sub-b", b.srv.URL, "secret-b-secret-b-secret-b-secret-b")
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(ownedHandoff(userU))
	waitFor(t, "both deliveries", func() bool { return a.count() == 1 && b.count() == 1 })
	for _, c := range []struct {
		cap    *capture
		secret string
	}{{a, "secret-a-secret-a-secret-a-secret-a"}, {b, "secret-b-secret-b-secret-b-secret-b"}} {
		body, hdr := c.cap.first()
		mac := hmac.New(sha256.New, []byte(c.secret))
		mac.Write(body)
		if got, want := hdr.Get("X-Cairn-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Fatalf("signature mismatch: got %q want %q", got, want)
		}
	}
}

// TestEventsGoOnlyToTheOwnersWorkspace is the routing rule behind "A friend
// comments on U's artifact" and "Team artifact": an event about U's artifact
// acted on by V reaches U's subscriptions and not V's; an event about team
// T's artifact reaches T's and not the acting member's.
func TestEventsGoOnlyToTheOwnersWorkspace(t *testing.T) {
	uRecv, vRecv, tRecv := newCapture(t, 200), newCapture(t, 200), newCapture(t, 200)
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "sub-u", uRecv.srv.URL, "u-secret-u-secret-u-secret-u-secret")
	subs.add(subscription.Owner{UserID: userV}, "sub-v", vRecv.srv.URL, "v-secret-v-secret-v-secret-v-secret")
	subs.add(subscription.Owner{TeamID: teamT}, "sub-t", tRecv.srv.URL, "t-secret-t-secret-t-secret-t-secret")
	e := newEmitter(t, subs)
	run(t, e)

	// V acts on U's artifact.
	ev := testEvent()
	ev.ActorID = "v@example.com"
	ev.OwnerUserID = userU
	e.EmitArtifactCreated(ev)
	// V, a member of T, creates into T.
	tev := testEvent()
	tev.PublicID = "team01"
	tev.ActorID = "v@example.com"
	tev.OwnerUserID, tev.OwnerTeamID = "", teamT
	e.EmitArtifactCreated(tev)

	waitFor(t, "U's and T's deliveries", func() bool { return uRecv.count() == 1 && tRecv.count() == 1 })
	time.Sleep(50 * time.Millisecond) // give a (wrong) delivery to V a chance
	if n := vRecv.count(); n != 0 {
		t.Fatalf("the actor's own subscription received %d events about artifacts it does not own", n)
	}
	var got eventBody
	body, _ := tRecv.first()
	if err := json.Unmarshal(body, &got); err != nil || got.Data.ID != "team01" {
		t.Fatalf("team subscription got %s, want the team artifact", body)
	}
}

// TestEventWithoutOwnerGoesNowhere: fail closed when an event has no owner.
func TestEventWithoutOwnerGoesNowhere(t *testing.T) {
	subs := &fakeSubs{}
	e := newEmitter(t, subs)
	run(t, e)
	ev := testEvent()
	ev.OwnerUserID = ""
	e.EmitArtifactCreated(ev)
	ev.OwnerUserID, ev.OwnerTeamID = userU, teamT // two owners is no owner
	e.EmitArtifactCreated(ev)
	time.Sleep(50 * time.Millisecond)
	subs.mu.Lock()
	defer subs.mu.Unlock()
	if len(subs.lookups) != 0 {
		t.Fatalf("looked up targets for an ownerless event: %v", subs.lookups)
	}
}

// TestFiltersSelectTargets: a subscription's filters decide whether it
// receives an event (the service applies them in SQL; this proves the
// emitter passes kind, share type and tags through).
func TestFiltersSelectTargets(t *testing.T) {
	match, miss := newCapture(t, 200), newCapture(t, 200)
	subs := &fakeSubs{}
	owner := subscription.Owner{UserID: userU}
	subs.subs = []fakeSub{
		{owner: owner, target: subscription.Target{ID: "m", URL: match.srv.URL, Secret: []byte("x")}, kinds: []string{"artifact.created"}, tags: []string{"handoff"}},
		{owner: owner, target: subscription.Target{ID: "n", URL: miss.srv.URL, Secret: []byte("y")}, tags: []string{"not-this-one"}},
	}
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(ownedHandoff(userU))
	waitFor(t, "the matching delivery", func() bool { return match.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if miss.count() != 0 {
		t.Fatal("a subscription whose tag filter does not match received the event")
	}
}

func TestRetryThenSuccessRecordsOneSuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", srv.URL, "k")
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d want 3", got)
	}
	if r := subs.recorded("s")[0]; !r.OK {
		t.Fatalf("recorded %+v, want success", r)
	}
}

func TestRetriesExhaustedRecordsOneFailure(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", srv.URL, "k")
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	if got := attempts.Load(); got != maxAttempts {
		t.Fatalf("attempts = %d want %d", got, maxAttempts)
	}
	if r := subs.recorded("s")[0]; r.OK || r.Status != 500 || r.Reason != subscription.ReasonHTTPStatus {
		t.Fatalf("recorded %+v, want one http_status failure", r)
	}
}

func TestClientErrorNoRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(410)
	}))
	defer srv.Close()
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", srv.URL, "k")
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	time.Sleep(50 * time.Millisecond) // give any (wrong) retry a chance
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d want 1 (4xx must not retry)", got)
	}
}

// TestRedirectIsAFailedDelivery is scenario "Redirect": a target answering
// 302 to another host is not followed, and the delivery counts as failed.
func TestRedirectIsAFailedDelivery(t *testing.T) {
	var hits, attempts atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer final.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer srv.Close()
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", srv.URL, "k")
	e := newEmitter(t, subs)
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	if attempts.Load() != 1 {
		t.Fatalf("a 3xx was retried: %d attempts", attempts.Load())
	}
	if r := subs.recorded("s")[0]; r.OK || r.Status != http.StatusFound || r.Reason != subscription.ReasonRedirect {
		t.Fatalf("recorded %+v, want a failed redirect", r)
	}
}

// TestBlockedAddressIsNotDialled: without the test permit, a loopback target
// is refused by the dialer and counted as a failure, with no retry.
func TestBlockedAddressIsNotDialled(t *testing.T) {
	recv := newCapture(t, 200)
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", recv.srv.URL, "k")
	old := backoffFor
	backoffFor = func(int) time.Duration { return 0 }
	t.Cleanup(func() { backoffFor = old })
	e := New(subs, &subscription.Policy{AllowHTTP: true}, "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	if recv.count() != 0 {
		t.Fatal("a loopback target was dialled")
	}
	if r := subs.recorded("s")[0]; r.OK || r.Reason != subscription.ReasonBlockedAddress {
		t.Fatalf("recorded %+v, want blocked_address", r)
	}
}

// TestHTTPTargetRefusedOnceHTTPIsOff: a stored http:// target is re-checked
// before every delivery, so turning CAIRN_OUTBOUND_ALLOW_HTTP off stops it.
func TestHTTPTargetRefusedOnceHTTPIsOff(t *testing.T) {
	recv := newCapture(t, 200)
	subs := &fakeSubs{}
	subs.add(subscription.Owner{UserID: userU}, "s", recv.srv.URL, "k")
	permit := loopbackPolicy()
	permit.AllowHTTP = false
	e := New(subs, permit, "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	run(t, e)
	e.EmitArtifactCreated(testEvent())
	waitFor(t, "a recorded result", func() bool { return len(subs.recorded("s")) == 1 })
	if recv.count() != 0 {
		t.Fatal("an http target was delivered to with http disallowed")
	}
	if r := subs.recorded("s")[0]; r.OK || r.Reason != subscription.ReasonTargetRefused {
		t.Fatalf("recorded %+v, want target_refused", r)
	}
}

func TestQueueFullDropsWithoutBlocking(t *testing.T) {
	e := newEmitter(t, &fakeSubs{})
	// Never run the worker: fill the queue past cap and assert Emit returns.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueCap+50; i++ {
			e.EmitArtifactCreated(testEvent())
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EmitArtifactCreated blocked on a full queue")
	}
}

// compile-time: Emitter satisfies the store hook, and the service is a
// Subscriptions.
var (
	_ store.CreationEmitter = (*Emitter)(nil)
	_ Subscriptions         = (*subscription.Service)(nil)
)
