package outboundhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012

func testEvent() store.CreationEvent {
	return store.CreationEvent{
		PublicID:  "abc123",
		ShareType: artifact.TypeFile,
		Title:     "hello",
		WebPath:   "/abc123",
		ActorID:   "joestump",
		Channel:   "api",
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC(),
	}
}

func newEmitter(t *testing.T, secret string, targets ...string) *Emitter {
	t.Helper()
	old := backoffFor
	backoffFor = func(int) time.Duration { return 0 }
	t.Cleanup(func() { backoffFor = old })
	return New(targets, secret, "https://cairn.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// runUntil delivers until n events have been received by the counter, or fails
// the test on timeout.
func runUntil(t *testing.T, e *Emitter, delivered *atomic.Int32, n int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	t.Cleanup(cancel)
	for {
		if delivered.Load() >= n {
			cancel()
			<-done
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d deliveries, got %d", n, delivered.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDeliveryPayloadAndHeaders(t *testing.T) {
	var delivered atomic.Int32
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, hdr = b, r.Header.Clone()
		delivered.Add(1)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	const secret = "s3cr3t"
	e := newEmitter(t, secret, srv.URL)
	e.EmitArtifactCreated(testEvent())
	runUntil(t, e, &delivered, 1)

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
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := hdr.Get("X-Cairn-Signature"); got != wantSig {
		t.Fatalf("signature mismatch: got %q want %q", got, wantSig)
	}
}

// TestSignatureCoversTaggedBody: the signature scheme is unchanged by the new
// fields — still HMAC-SHA256 over the exact delivered bytes — and those bytes
// carry tags and on_behalf_of verbatim.
func TestSignatureCoversTaggedBody(t *testing.T) {
	var delivered atomic.Int32
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, hdr = b, r.Header.Clone()
		delivered.Add(1)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	const secret = "s3cr3t"
	e := newEmitter(t, secret, srv.URL)
	e.EmitArtifactCreated(handoffEvent())
	runUntil(t, e, &delivered, 1)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if got, want := hdr.Get("X-Cairn-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("signature mismatch over tagged body: got %q want %q", got, want)
	}
	var got eventBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got.Data.OnBehalfOf != "claude-code/2.1.0" {
		t.Fatalf("on_behalf_of = %q", got.Data.OnBehalfOf)
	}
	if len(got.Data.Tags) != 7 || got.Data.Tags[0] != "handoff" || got.Data.Tags[4] != "issue:stump.wtf/cairn#42" {
		t.Fatalf("tags = %q", got.Data.Tags)
	}
}

func TestNoSecretOmitsSignature(t *testing.T) {
	var delivered atomic.Int32
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		delivered.Add(1)
	}))
	defer srv.Close()

	e := newEmitter(t, "", srv.URL)
	e.EmitArtifactCreated(testEvent())
	runUntil(t, e, &delivered, 1)
	if hdr.Get("X-Cairn-Signature") != "" {
		t.Fatal("signature header present without secret")
	}
}

func TestMultipleTargetsEachReceive(t *testing.T) {
	var a, b atomic.Int32
	mk := func(c *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Add(1)
		}))
	}
	sa, sb := mk(&a), mk(&b)
	defer sa.Close()
	defer sb.Close()

	e := newEmitter(t, "", sa.URL, sb.URL)
	e.EmitArtifactCreated(testEvent())
	runUntil(t, e, &a, 1)
	runUntil(t, e, &b, 1)
}

func TestRetryThenSuccess(t *testing.T) {
	var attempts, delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(500)
			return
		}
		delivered.Add(1)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	e := newEmitter(t, "", srv.URL)
	e.EmitArtifactCreated(testEvent())
	runUntil(t, e, &delivered, 1)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d want 3", got)
	}
}

func TestRetriesExhaustedDrops(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	e := newEmitter(t, "", srv.URL)
	e.EmitArtifactCreated(testEvent())
	// Drive Run directly with a short context; the event must be attempted
	// exactly maxAttempts times then abandoned (no infinite retry).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	for attempts.Load() < maxAttempts && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := attempts.Load(); got != maxAttempts {
		t.Fatalf("attempts = %d want %d", got, maxAttempts)
	}
}

func TestClientErrorNoRetry(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(410)
	}))
	defer srv.Close()

	e := newEmitter(t, "", srv.URL)
	e.EmitArtifactCreated(testEvent())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	for ctx.Err() == nil && attempts.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give any (wrong) retry a chance
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d want 1 (4xx must not retry)", got)
	}
}

func TestNoRedirectFollowing(t *testing.T) {
	var hits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer final.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer srv.Close()

	e := newEmitter(t, "", srv.URL)
	e.EmitArtifactCreated(testEvent())
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	<-ctx.Done()
	<-done
	if hits.Load() != 0 {
		t.Fatal("client followed a redirect")
	}
}

func TestQueueFullDropsWithoutBlocking(t *testing.T) {
	e := newEmitter(t, "", "http://127.0.0.1:0/not-running")
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

// compile-time: Emitter satisfies the store hook.
var _ store.CreationEmitter = (*Emitter)(nil)
