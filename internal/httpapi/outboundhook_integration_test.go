package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/subscription"
	"github.com/stump-wtf/cairn/internal/user"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Emission on
// Artifact Creation" + "Signed Delivery" + "Event Payload"; SPEC-0023 REQ
// "Owned Outbound Subscriptions", REQ "Events Go Only to the Artifact's
// Workspace", REQ "Subscription Target Safety"

// testSubsKey seals subscription secrets in the integration suite.
var testSubsKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))

// testSubsPolicy admits the suite's loopback httptest receivers over http;
// everything else follows the production address rules.
func testSubsPolicy() *subscription.Policy {
	return &subscription.Policy{
		AllowHTTP:  true,
		PermitAddr: func(a netip.Addr) bool { return a.IsLoopback() || subscription.PublicAddr(a) },
	}
}

// newTestSubscriptions builds the subscription core over pool; withKey false
// leaves it without CAIRN_ENCRYPTION_KEY.
func newTestSubscriptions(pool *pgxpool.Pool, policy *subscription.Policy, withKey bool) *subscription.Service {
	opts := subscription.Options{Policy: policy}
	if withKey {
		opts.Sealer, _ = subscription.ParseKey(testSubsKey)
	}
	return subscription.NewService(pool, opts)
}

// wireSubscriptions gives a test server what cairnd gives it: one
// subscription core shared by the /v1 surface and a running emitter the store
// hands every creation to.
func wireSubscriptions(t *testing.T, pool *pgxpool.Pool, cfg *Config, opts *store.Options) {
	t.Helper()
	if cfg.Subscriptions == nil {
		cfg.Subscriptions = newTestSubscriptions(pool, testSubsPolicy(), true)
	}
	if opts.Emitter != nil {
		return
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://cairn.test"
	}
	em := outboundhook.New(cfg.Subscriptions, cfg.Subscriptions.Policy(), strings.TrimRight(base, "/"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); em.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	opts.Emitter = em
}

// testSubs is a test server's subscription core and the pool it runs on.
type testSubs struct {
	*subscription.Service
	pool *pgxpool.Pool
}

// subscribe gives actor (as the dev bearer resolves it) a subscription to
// target directly through the core, returning its id and secret.
func subscribe(t *testing.T, subs *testSubs, actor, target string, in subscription.CreateInput) (string, string) {
	t.Helper()
	u := actorUser(t, subs, actor)
	in.Owner, in.CreatedBy, in.URL = subscription.Owner{UserID: u}, u, target
	secret, sub, err := subs.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("subscribe %s: %v", actor, err)
	}
	return sub.ID, secret
}

// actorUser resolves actor to its user id the way the dev bearer does.
func actorUser(t *testing.T, subs *testSubs, actor string) string {
	t.Helper()
	u, err := user.NewStore(subs.pool).ResolveActor(context.Background(), actor)
	if err != nil {
		t.Fatalf("ResolveActor(%s): %v", actor, err)
	}
	return u.ID
}

type hookedEvent struct {
	body []byte
	hdr  http.Header
}

// hookReceiver stands in for a Switchboard ingest endpoint.
type hookReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []hookedEvent
	ready  chan struct{} // closed after the first delivery
	status int
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{ready: make(chan struct{}), status: http.StatusAccepted}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.seen = append(r.seen, hookedEvent{body: b, hdr: req.Header.Clone()})
		status := r.status
		r.mu.Unlock()
		select {
		case <-r.ready:
		default:
			close(r.ready)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *hookReceiver) wait(t *testing.T) hookedEvent {
	t.Helper()
	select {
	case <-r.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook delivery arrived")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[0]
}

func (r *hookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// verifySignature checks X-Cairn-Signature over the delivered bytes.
func verifySignature(t *testing.T, ev hookedEvent, secret string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(ev.body)
	if got, want := ev.hdr.Get("X-Cairn-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("signature mismatch: got %q want %q", got, want)
	}
}

// TestIntegrationArtifactCreateEmitsSignedWebhook proves the full path: a REST
// artifact creation (the same store choke point the MCP and CLI surfaces use)
// produces an artifact.created delivery to the creator's subscription, signed
// with that subscription's own minted secret, and a 201 is returned
// regardless of the emitter.
func TestIntegrationArtifactCreateEmitsSignedWebhook(t *testing.T) {
	recv := newHookReceiver(t)
	srv, subs := subsTestServer(t, Config{BaseURL: "https://cairn.test", DevInsecureBearerAuth: true}, store.Options{}, nil)
	_, secret := subscribe(t, subs, "joestump", recv.srv.URL, subscription.CreateInput{})
	id := createArtifact(t, srv.URL, "text", "joestump", "hello webhook world")

	ev := recv.wait(t)
	var body struct {
		Source  string `json:"source"`
		Kind    string `json:"kind"`
		EventID string `json:"event_id"`
		Data    struct {
			ID        string `json:"id"`
			ShareType string `json:"share_type"`
			URL       string `json:"url"`
			ActorID   string `json:"actor_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(ev.body, &body); err != nil {
		t.Fatalf("event body not JSON: %v", err)
	}
	if body.Source != "cairn" || body.Kind != "artifact.created" || body.EventID == "" {
		t.Fatalf("bad envelope: %+v", body)
	}
	if body.Data.ID != id || body.Data.ShareType == "" {
		t.Fatalf("event data mismatch: %+v (created %s)", body.Data, id)
	}
	if want := "https://cairn.test/" + id; body.Data.URL != want {
		t.Fatalf("url = %q want %q", body.Data.URL, want)
	}
	if got := ev.hdr.Get("X-Cairn-Event-Id"); got != body.EventID {
		t.Fatalf("X-Cairn-Event-Id = %q want %q", got, body.EventID)
	}
	verifySignature(t, ev, secret)
}

// TestIntegrationNoSubscriptionDeliversNothing: a user with no subscription
// sends nothing anywhere, and creation behaves exactly as before.
func TestIntegrationNoSubscriptionDeliversNothing(t *testing.T) {
	recv := newHookReceiver(t)
	srv, subs := subsTestServer(t, Config{DevInsecureBearerAuth: true}, store.Options{}, nil)
	subscribe(t, subs, "someone-else", recv.srv.URL, subscription.CreateInput{})
	if id := createArtifact(t, srv.URL, "text", "joestump", "no subscription"); id == "" {
		t.Fatal("creation failed")
	}
	time.Sleep(200 * time.Millisecond)
	if n := recv.count(); n != 0 {
		t.Fatalf("another user's subscription received %d events about joestump's artifact", n)
	}
}

// TestIntegrationEventsGoOnlyToTheOwner: V's subscription receives nothing
// about U's artifacts, and U's receives nothing about V's (REQ "Events Go
// Only to the Artifact's Workspace").
func TestIntegrationEventsGoOnlyToTheOwner(t *testing.T) {
	uRecv, vRecv := newHookReceiver(t), newHookReceiver(t)
	srv, subs := subsTestServer(t, Config{DevInsecureBearerAuth: true}, store.Options{}, nil)
	subscribe(t, subs, "u@example.com", uRecv.srv.URL, subscription.CreateInput{})
	subscribe(t, subs, "v@example.com", vRecv.srv.URL, subscription.CreateInput{})

	uID := createArtifact(t, srv.URL, "text", "u@example.com", "u's artifact")
	vID := createArtifact(t, srv.URL, "text", "v@example.com", "v's artifact")
	for _, c := range []struct {
		recv *hookReceiver
		want string
	}{{uRecv, uID}, {vRecv, vID}} {
		ev := c.recv.wait(t)
		var body struct {
			Data struct{ ID string } `json:"data"`
		}
		_ = json.Unmarshal(ev.body, &body)
		if body.Data.ID != c.want {
			t.Fatalf("a subscription received %s, want only %s", body.Data.ID, c.want)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if uRecv.count() != 1 || vRecv.count() != 1 {
		t.Fatalf("deliveries: U=%d V=%d, want exactly one each", uRecv.count(), vRecv.count())
	}
}

// TestIntegrationRebindingTargetFails is scenario "Rebinding target" end to
// end: a target that resolved to a public address when it was created and to
// an RFC 1918 address at delivery is not dialled, and the subscription's
// health records a failure.
func TestIntegrationRebindingTargetFails(t *testing.T) {
	res := &flipResolver{addr: netip.MustParseAddr("93.184.216.34")}
	policy := testSubsPolicy()
	policy.Resolver = res
	srv, subs := subsTestServer(t, Config{DevInsecureBearerAuth: true}, store.Options{}, policy)
	subID, _ := subscribe(t, subs, "u@example.com", "https://rebind.example.com/hook", subscription.CreateInput{})

	res.set(netip.MustParseAddr("10.0.0.7"))
	createArtifact(t, srv.URL, "text", "u@example.com", "after the rebind")
	sub := waitHealth(t, subs, "u@example.com", subID)
	if sub.ConsecutiveFailures != 1 || sub.LastError != subscription.ReasonBlockedAddress || sub.LastStatus != nil {
		t.Fatalf("health = %d failures, %q, status %v; want one blocked_address failure with no response",
			sub.ConsecutiveFailures, sub.LastError, sub.LastStatus)
	}
}

// TestIntegrationRedirectIsFailedDelivery is scenario "Redirect" end to end.
func TestIntegrationRedirectIsFailedDelivery(t *testing.T) {
	final := newHookReceiver(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.srv.URL, http.StatusFound)
	}))
	defer redirector.Close()
	srv, subs := subsTestServer(t, Config{DevInsecureBearerAuth: true}, store.Options{}, nil)
	subID, _ := subscribe(t, subs, "u@example.com", redirector.URL, subscription.CreateInput{})

	createArtifact(t, srv.URL, "text", "u@example.com", "redirected")
	sub := waitHealth(t, subs, "u@example.com", subID)
	if sub.ConsecutiveFailures != 1 || sub.LastError != subscription.ReasonRedirect || sub.LastStatus == nil || *sub.LastStatus != http.StatusFound {
		t.Fatalf("health = %+v; want one failed delivery with status 302", sub)
	}
	if final.count() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// waitHealth waits until the subscription records a delivery attempt.
func waitHealth(t *testing.T, subs *testSubs, actor, id string) *subscription.Subscription {
	t.Helper()
	owner := subscription.Owner{UserID: actorUser(t, subs, actor)}
	deadline := time.Now().Add(10 * time.Second)
	for {
		sub, err := subs.Get(context.Background(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if sub.LastAttemptAt != nil {
			return sub
		}
		if time.Now().After(deadline) {
			t.Fatal("no delivery attempt was recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// flipResolver answers one address for every host, switchable mid-test.
type flipResolver struct {
	mu   sync.Mutex
	addr netip.Addr
}

func (f *flipResolver) set(a netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addr = a
}

func (f *flipResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []netip.Addr{f.addr}, nil
}
