package httpapi

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
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012 REQ "Event Emission on
// Artifact Creation" + "Signed Delivery" + "Event Payload"

type hookedEvent struct {
	body []byte
	hdr  http.Header
}

// hookReceiver stands in for a Switchboard ingest endpoint.
type hookReceiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	seen  []hookedEvent
	ready chan struct{} // closed after the first delivery
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{ready: make(chan struct{})}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.seen = append(r.seen, hookedEvent{body: b, hdr: req.Header.Clone()})
		r.mu.Unlock()
		select {
		case <-r.ready:
		default:
			close(r.ready)
		}
		w.WriteHeader(http.StatusAccepted)
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

// TestIntegrationArtifactCreateEmitsSignedWebhook proves the full path: a REST
// artifact creation (the same store choke point the MCP and CLI surfaces use)
// produces a signed artifact.created delivery to a configured target, and that
// a 201 is returned regardless of the emitter.
func TestIntegrationArtifactCreateEmitsSignedWebhook(t *testing.T) {
	recv := newHookReceiver(t)
	const secret = "test-secret"

	opts := store.Options{}
	em := outboundhook.New([]string{recv.srv.URL}, secret, "https://cairn.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	opts.Emitter = em

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); em.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	srv := testServer(t, Config{}, opts)
	id := createArtifact(t, srv.URL, "text", "joestump", "hello webhook world")

	ev := recv.wait(t)
	var body struct {
		Source    string `json:"source"`
		Kind      string `json:"kind"`
		EventID   string `json:"event_id"`
		CreatedAt string `json:"created_at"`
		Data      struct {
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
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(ev.body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := ev.hdr.Get("X-Cairn-Signature"); got != wantSig {
		t.Fatalf("signature mismatch: got %q want %q", got, wantSig)
	}
}

// TestIntegrationNoEmitterIsInert: with no emitter configured (the default),
// creation behaves exactly as before — the store-level nil guard.
func TestIntegrationNoEmitterIsInert(t *testing.T) {
	srv := testServer(t, Config{}, store.Options{})
	id := createArtifact(t, srv.URL, "text", "joestump", "no emitter")
	if id == "" {
		t.Fatal("creation failed without emitter")
	}
}
