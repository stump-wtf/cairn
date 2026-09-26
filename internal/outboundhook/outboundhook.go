// Package outboundhook delivers post-commit artifact events to the owned
// outbound subscriptions of the workspace that owns the artifact. It is the
// producer-side twin of the inbound Webhook Inspector (internal/webhook,
// SPEC-0005): that feature captures inbound requests as artifacts; this one
// announces new artifacts to the outside world, notably Switchboard ingest
// URLs that turn events into durable todos.
//
// Targets are rows, never configuration: each event carries the owner of its
// subject artifact, and a worker asks internal/subscription for that
// workspace's active subscriptions whose filters admit it. Each delivery is
// signed with that subscription's own secret, dialled through the target
// policy (which re-checks the resolved address on every dial and never
// follows a redirect), and its outcome is recorded as the subscription's
// health. There is no instance-wide target list.
//
// Delivery is deliberately best-effort: a bounded in-memory queue, a few
// workers, and three attempts per subscription. Events in flight at process
// death are lost — the artifact URL remains the record; the doorbell is a
// hint.
//
// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012; ADR-0029 (section 6),
// SPEC-0023 REQ "Owned Outbound Subscriptions", REQ "Events Go Only to the
// Artifact's Workspace", REQ "Subscription Target Safety"
package outboundhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/subscription"
)

// Tunables. The queue cap bounds memory under burst; the retry schedule is
// short because consumers are few and internal (SPEC-0012 REQ "Bounded Async
// Delivery with Retry"). The per-attempt timeout is the target policy's
// (subscription.AttemptTimeout, 5 seconds).
const (
	queueCap    = 256
	maxAttempts = 3
	// workers deliver concurrently, so one slow subscription (up to three
	// 5-second attempts) does not hold every other owner's events behind it.
	workers = 4
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

// Subscriptions is the target source: internal/subscription.Service.
type Subscriptions interface {
	Targets(ctx context.Context, owner subscription.Owner, m subscription.Match) ([]subscription.Target, int, error)
	Record(ctx context.Context, id string, r subscription.Result) (bool, error)
}

// Emitter implements store.CreationEmitter.
type Emitter struct {
	subs    Subscriptions
	policy  *subscription.Policy
	baseURL string
	log     *slog.Logger
	client  *http.Client
	ch      chan envelope
}

type envelope struct {
	id        string
	createdAt time.Time
	body      []byte
	// owner and match select the targets; neither is on the wire.
	owner subscription.Owner
	match subscription.Match
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

// New builds an emitter delivering to subs' targets through policy, whose
// client dials every attempt. baseURL is the public origin joined onto the
// store-provided web path. Call Run to start delivery.
func New(subs Subscriptions, policy *subscription.Policy, baseURL string, log *slog.Logger) *Emitter {
	if policy == nil {
		policy = &subscription.Policy{}
	}
	return &Emitter{
		subs:    subs,
		policy:  policy,
		baseURL: baseURL,
		log:     log,
		ch:      make(chan envelope, queueCap),
		client:  policy.Client(),
	}
}

// EmitArtifactCreated enqueues an event without ever blocking the caller. On a
// full queue the event is dropped with a warning — the doorbell is a hint, not
// a ledger (SPEC-0012 REQ "Bounded Async Delivery with Retry"). The event
// goes only to the artifact owner's subscriptions, never the actor's unless
// the actor is the owner (SPEC-0023 REQ "Events Go Only to the Artifact's
// Workspace"); the lookup runs on a worker, so creation never waits on it.
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
	owner := subscription.Owner{UserID: ev.OwnerUserID, TeamID: ev.OwnerTeamID}
	if !owner.Valid() {
		// An artifact always has exactly one owner (SPEC-0023 REQ "Owner
		// Model"); an event without one has nowhere it may go.
		return
	}
	eventID, createdAt := uuid.NewString(), time.Now().UTC()
	raw, err := e.encode(ev, eventID, createdAt)
	if err != nil {
		// Marshal of this shape cannot fail; log defensively rather than panic.
		e.log.Error("outboundhook: marshal event", "error", err)
		return
	}
	env := envelope{
		id: eventID, createdAt: createdAt, body: raw, owner: owner,
		match: subscription.Match{Kind: EventKind, ShareType: string(ev.ShareType), Tags: ev.Tags},
	}
	select {
	case e.ch <- env:
	default:
		e.log.Warn("outboundhook: queue full, dropping event",
			"event_id", eventID, "queue_cap", queueCap)
	}
}

// encode renders the wire body for one event — the exact bytes
// X-Cairn-Signature covers. It takes the event id and clock as arguments so
// golden tests can pin those bytes, which is how a payload change is proven
// additive rather than merely asserted to be. The owner is never encoded.
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
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case env := <-e.ch:
					e.deliver(ctx, env)
				}
			}
		}()
	}
	wg.Wait()
}

// deliver fans one event out to the owning workspace's matching
// subscriptions, each independently retried and recorded. Target URLs are
// bearer capabilities, so only subscription ids are ever logged.
func (e *Emitter) deliver(ctx context.Context, env envelope) {
	if e.subs == nil {
		return
	}
	targets, skipped, err := e.subs.Targets(ctx, env.owner, env.match)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("outboundhook: look up subscriptions", "event_id", env.id, "error", err)
		}
		return
	}
	if skipped > 0 {
		e.log.Error("outboundhook: subscription secrets do not open under CAIRN_ENCRYPTION_KEY; not delivering to them",
			"event_id", env.id, "skipped", skipped)
	}
	for _, t := range targets {
		res := e.deliverOne(ctx, env, t)
		if ctx.Err() != nil {
			// Shutdown interrupted the attempt: that is not the target's fault.
			return
		}
		disabled, err := e.subs.Record(ctx, t.ID, res)
		if err != nil {
			e.log.Error("outboundhook: record delivery", "subscription_id", t.ID, "event_id", env.id, "error", err)
		}
		if !res.OK {
			e.log.Warn("outboundhook: delivery failed",
				"subscription_id", t.ID, "event_id", env.id, "reason", res.Reason, "status", res.Status)
		}
		if disabled {
			e.log.Warn("outboundhook: subscription disabled after consecutive failures",
				"subscription_id", t.ID, "failures", subscription.DisableAfter)
		}
	}
}

// deliverOne posts one event to one subscription with retries, signed with
// that subscription's secret (SPEC-0012 REQ "Signed Delivery"). The target's
// shape is re-checked first, so turning CAIRN_OUTBOUND_ALLOW_HTTP off stops
// http:// targets at once; its addresses are re-checked by the policy's
// dialer on every attempt.
func (e *Emitter) deliverOne(ctx context.Context, env envelope, t subscription.Target) subscription.Result {
	if err := e.policy.CheckTarget(t.URL); err != nil {
		return subscription.Result{Reason: subscription.ReasonTargetRefused}
	}
	mac := hmac.New(sha256.New, t.Secret)
	mac.Write(env.body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	var last subscription.Result
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
			return last
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(env.body))
		if err != nil {
			return subscription.Result{Reason: subscription.ReasonTargetRefused}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "cairn-outboundhook/1")
		req.Header.Set("X-Cairn-Event", EventKind)
		req.Header.Set("X-Cairn-Event-Id", env.id)
		req.Header.Set("X-Cairn-Signature", sig)
		resp, err := e.client.Do(req)
		if err != nil {
			last = subscription.Result{Reason: failureReason(err)}
			if last.Reason == subscription.ReasonBlockedAddress {
				// The address will not become public on a retry.
				return last
			}
			continue
		}
		_ = resp.Body.Close()
		switch code := resp.StatusCode; {
		case code >= 200 && code < 300:
			return subscription.Result{OK: true, Status: code}
		case code >= 300 && code < 400:
			// Redirects are never followed: a 3xx is a failed delivery, and a
			// retry would only be redirected again (SPEC-0023 REQ
			// "Subscription Target Safety").
			return subscription.Result{Status: code, Reason: subscription.ReasonRedirect}
		case code >= 400 && code < 500 && code != http.StatusTooManyRequests:
			// 4xx (other than 429) will not improve on retry; abandon early.
			return subscription.Result{Status: code, Reason: subscription.ReasonHTTPStatus}
		default:
			last = subscription.Result{Status: code, Reason: subscription.ReasonHTTPStatus}
		}
	}
	return last
}

// failureReason classifies a transport error into the fixed vocabulary
// recorded as last_error, which never carries a host or URL.
func failureReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, subscription.ErrBlockedAddress):
		return subscription.ReasonBlockedAddress
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return subscription.ReasonTimeout
	default:
		return subscription.ReasonConnection
	}
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
