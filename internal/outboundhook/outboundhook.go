// Package outboundhook delivers post-commit artifact-creation events to
// configured HTTP targets (outbound webhooks). It is the producer-side twin of
// the inbound Webhook Inspector (internal/webhook, SPEC-0005): that feature
// captures inbound requests as artifacts; this one announces new artifacts to
// the outside world, notably Switchboard ingest URLs that turn events into
// durable todos.
//
// Delivery is deliberately best-effort: a bounded in-memory queue and one
// worker goroutine with three attempts per target. Events in flight at process
// death are lost — the artifact URL remains the record; the doorbell is a hint.
//
// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012
package outboundhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/joestump/cairn/internal/store"
)

// Tunables. The queue cap bounds memory under burst; the retry schedule is
// short because consumers are few and internal (SPEC-0012 REQ "Bounded Async
// Delivery with Retry").
const (
	queueCap       = 256
	maxAttempts    = 3
	requestTimeout = 5 * time.Second
)

// backoffFor returns the wait before attempt n (0-based). Package-level so
// tests can shorten it.
var backoffFor = func(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 0
	case 1:
		return time.Second
	default:
		return 4 * time.Second
	}
}

// EventKind is the sole event kind today; the envelope leaves room for more.
const EventKind = "artifact.created"

// Emitter implements store.CreationEmitter.
type Emitter struct {
	targets []string
	secret  []byte
	baseURL string
	log     *slog.Logger
	client  *http.Client
	ch      chan envelope
}

type envelope struct {
	id        string
	createdAt time.Time
	body      []byte
}

// eventBody is the wire format (SPEC-0012 REQ "Event Payload").
type eventBody struct {
	Source    string    `json:"source"`
	Kind      string    `json:"kind"`
	EventID   string    `json:"event_id"`
	CreatedAt time.Time `json:"created_at"`
	Data      eventData `json:"data"`
}

type eventData struct {
	ID        string     `json:"id"`
	ShareType string     `json:"share_type"`
	Title     string     `json:"title,omitempty"`
	URL       string     `json:"url"`
	Channel   string     `json:"channel,omitempty"`
	Model     string     `json:"model,omitempty"`
	ActorID   string     `json:"actor_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Appended after the original fields and omitted when empty, so the event
	// for an untagged REST/CLI artifact stays byte-identical to the payload
	// before these existed (pinned by testdata/artifact_created_rest.golden.json).
	//
	// OnBehalfOf is recorded by the server from the MCP session handshake, never
	// from a tool argument, but its content is the client's self-reported
	// name/version: it names the harness, not a principal. ActorID is the only
	// authenticated identity. Tags are client-asserted: a consumer routes on
	// them, never authorizes on them (ADR-0018). They arrive already normalized, in
	// the stored order, so the signed bytes are deterministic.
	OnBehalfOf string   `json:"on_behalf_of,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// New builds an emitter delivering to each target URL. baseURL is the public
// origin joined onto the store-provided web path; secret, when non-empty,
// signs every delivery with X-Cairn-Signature. Call Run to start delivery.
func New(targets []string, secret, baseURL string, log *slog.Logger) *Emitter {
	e := &Emitter{
		targets: append([]string(nil), targets...),
		baseURL: baseURL,
		log:     log,
		ch:      make(chan envelope, queueCap),
		client: &http.Client{
			Timeout: requestTimeout,
			// Never follow redirects: a redirecting target would leak the
			// signed body (and the capability URL) to an unconfigured host
			// (SPEC-0012 Security Requirements — redirect validation).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if secret != "" {
		e.secret = []byte(secret)
	}
	return e
}

// EmitArtifactCreated enqueues an event without ever blocking the caller. On a
// full queue the event is dropped with a warning — the doorbell is a hint, not
// a ledger (SPEC-0012 REQ "Bounded Async Delivery with Retry").
func (e *Emitter) EmitArtifactCreated(ev store.CreationEvent) {
	// A nil receiver is a no-op rather than a panic. store.Options.Emitter is an
	// INTERFACE field, so a nil *Emitter assigned into it yields a non-nil
	// interface wrapping a nil pointer: store's `s.emitter == nil` guard reads
	// false and dispatches here anyway. That panicked on every artifact create,
	// post-commit — the row and blob were written and the caller still got a 500
	// (cairn#201). main() no longer hands over a typed nil, and this guard is the
	// other half: it keeps the panic unreachable if any future caller does.
	if e == nil {
		return
	}
	eventID, createdAt := uuid.NewString(), time.Now().UTC()
	raw, err := e.encode(ev, eventID, createdAt)
	if err != nil {
		// Marshal of this shape cannot fail; log defensively rather than panic.
		e.log.Error("outboundhook: marshal event", "error", err)
		return
	}
	select {
	case e.ch <- envelope{id: eventID, createdAt: createdAt, body: raw}:
	default:
		e.log.Warn("outboundhook: queue full, dropping event",
			"event_id", eventID, "queue_cap", queueCap)
	}
}

// encode renders the wire body for one event — the exact bytes
// X-Cairn-Signature covers. It takes the event id and clock as arguments so
// golden tests can pin those bytes, which is how a payload change is proven
// additive rather than merely asserted to be.
func (e *Emitter) encode(ev store.CreationEvent, eventID string, createdAt time.Time) ([]byte, error) {
	body := eventBody{
		Source:    "cairn",
		Kind:      EventKind,
		EventID:   eventID,
		CreatedAt: createdAt,
		Data: eventData{
			ID:         ev.PublicID,
			ShareType:  string(ev.ShareType),
			Title:      ev.Title,
			URL:        e.baseURL + ev.WebPath,
			Channel:    ev.Channel,
			Model:      ev.Model,
			ActorID:    ev.ActorID,
			OnBehalfOf: ev.OnBehalfOf,
			Tags:       ev.Tags,
		},
	}
	if !ev.ExpiresAt.IsZero() {
		t := ev.ExpiresAt.UTC()
		body.Data.ExpiresAt = &t
	}
	return json.Marshal(body)
}

// Run delivers queued events until ctx is cancelled. Start it as a goroutine,
// reaper-style; it returns when ctx is done and in-flight attempts unwind.
func (e *Emitter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-e.ch:
			e.deliver(ctx, env)
		}
	}
}

// deliver fans one event out to every target, independently retried.
func (e *Emitter) deliver(ctx context.Context, env envelope) {
	for i, target := range e.targets {
		if err := e.deliverOne(ctx, env, target); err != nil {
			// Target URLs are bearer capabilities: never log them (SPEC-0012
			// REQ "Delivery Targets from Configuration").
			e.log.Error("outboundhook: delivery abandoned",
				"target_index", i, "event_id", env.id, "attempts", maxAttempts, "error", err)
		}
	}
}

func (e *Emitter) deliverOne(ctx context.Context, env envelope, target string) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(env.body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "cairn-outboundhook/1")
		req.Header.Set("X-Cairn-Event", EventKind)
		req.Header.Set("X-Cairn-Event-Id", env.id)
		if e.secret != nil {
			mac := hmac.New(sha256.New, e.secret)
			mac.Write(env.body)
			req.Header.Set("X-Cairn-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt+1, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		// 4xx (other than 429) will not improve on retry; abandon early.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("attempt %d: status %d", attempt+1, resp.StatusCode)
		}
		lastErr = fmt.Errorf("attempt %d: status %d", attempt+1, resp.StatusCode)
	}
	return lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
