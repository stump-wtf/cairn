// Package outboundhook delivers post-commit lifecycle events to the owned
// outbound subscriptions of the workspace that owns the artifact each event
// is about. It is the producer-side twin of the inbound Webhook Inspector
// (internal/webhook, SPEC-0005): that feature captures inbound requests as
// artifacts; this one announces what happens to artifacts to the outside
// world, notably Switchboard ingest URLs that turn events into durable todos.
//
// It is the only encoder of the SPEC-0016 event kinds: every kind in the
// event registry renders here, on the one ADR-0017 envelope. Targets are rows,
// never configuration: each event carries the owner of its subject artifact,
// and a worker asks internal/subscription for that workspace's active
// subscriptions whose filters admit the event's kind, share type and tags
// (SPEC-0016 EV-7). Each delivery is signed with that subscription's own
// secret, dialled through the target policy (which re-checks the resolved
// address on every dial and never follows a redirect), and its outcome is
// recorded as the subscription's health. There is no instance-wide target
// list.
//
// Delivery is deliberately best-effort: a bounded in-memory queue, a few
// workers, and three attempts per subscription. Once the queue is more than
// half full, events other than artifact.created are dropped and counted
// before they are enqueued, so a burst of reactions cannot starve creations
// (EV-8). Events in flight at process death are lost — the artifact URL
// remains the record; the doorbell is a hint.
//
// Governing: ADR-0017 (Outbound Webhooks), SPEC-0012; ADR-0022, SPEC-0016
// EV-1, EV-3, EV-7, EV-8; ADR-0029 (section 6), SPEC-0023 REQ "Owned Outbound
// Subscriptions", REQ "Events Go Only to the Artifact's Workspace", REQ
// "Subscription Target Safety"
package outboundhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/stump-wtf/cairn/internal/event"
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
	// priorityDepth is the queue depth past which only artifact.created is
	// enqueued (SPEC-0016 EV-8 "Reaction burst does not starve creation").
	priorityDepth = queueCap / 2
)

// commentBodyMax caps data.comment.body in bytes. The stored comment is never
// touched; only the event copy is cut (SPEC-0016 EV-3).
const commentBodyMax = 4096

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

// Sentinel errors for the ways encode refuses an event (SPEC-0016 "Error
// Handling Standards"). Each is logged by Emit with the kind and event id, and
// nothing is delivered.
var (
	// ErrUnknownKind: the kind is not in the EV-1 registry. The reserved
	// comment.edited and comment.deleted are refused too.
	ErrUnknownKind = errors.New("outboundhook: unknown event kind")
	// ErrInvalidActor: the actor is not fully server-derived (EV-4), so the
	// event cannot say who caused it.
	ErrInvalidActor = errors.New("outboundhook: event actor is not server-derived")
	// ErrPayloadMismatch: the kind's payload object is missing, or another
	// kind's is set (EV-3).
	ErrPayloadMismatch = errors.New("outboundhook: event payload does not match its kind")
)

// Subscriptions is the target source: internal/subscription.Service.
type Subscriptions interface {
	Targets(ctx context.Context, owner subscription.Owner, m subscription.Match) ([]subscription.Target, int, error)
	Record(ctx context.Context, id string, r subscription.Result) (bool, error)
}

// Emitter implements event.Emitter, and store.CreationEmitter as a thin
// adapter over it.
type Emitter struct {
	subs    Subscriptions
	policy  *subscription.Policy
	baseURL string
	log     *slog.Logger
	client  *http.Client
	ch      chan envelope
	// dropped counts events not enqueued, per registered kind. The map is
	// built once in New and only read afterwards, so the counters need no lock.
	dropped map[event.Kind]*atomic.Uint64
}

type envelope struct {
	id        string
	kind      event.Kind
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

// eventData is data for every kind. The first block describes the subject
// artifact and the actor; its order is fixed, because consumers verify a
// signature over these exact bytes (SPEC-0016 EV-3).
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
	// for an untagged REST/CLI artifact differs from the payload before these
	// existed only by the ADR-0022 keys below (pinned by
	// testdata/artifact_created_rest.golden.json).
	//
	// OnBehalfOf is recorded by the server from the MCP session handshake, never
	// from a tool argument, but its content is the client's self-reported
	// name/version: it names the harness, not a principal. ActorID is the only
	// authenticated identity. Tags are client-asserted: a consumer routes on
	// them, never authorizes on them (ADR-0018). They arrive already normalized, in
	// the stored order, so the signed bytes are deterministic.
	OnBehalfOf string   `json:"on_behalf_of,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	// ADR-0022 additions, appended. ActorKind and Auth are server-derived and
	// present on every kind, because encode refuses an actor without them
	// (EV-4). At most one of Comment, Reaction and Run is set, by kind.
	ActorKind string        `json:"actor_kind,omitempty"`
	Auth      string        `json:"auth,omitempty"`
	Comment   *commentData  `json:"comment,omitempty"`
	Reaction  *reactionData `json:"reaction,omitempty"`
	Run       *runData      `json:"run,omitempty"`
}

type commentData struct {
	ID         int64  `json:"id"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	AnchorType string `json:"anchor_type"`
	AnchorKey  string `json:"anchor_key"`
	Body       string `json:"body"`
	// BodyTruncated is omitted unless the body was cut (EV-3's omit rule).
	BodyTruncated bool `json:"body_truncated,omitempty"`
}

type reactionData struct {
	ID         int64  `json:"id"`
	AnchorType string `json:"anchor_type"`
	AnchorKey  string `json:"anchor_key"`
	Emoji      string `json:"emoji"`
	// Always present: false is meaningful here (EV-3, EV-5).
	ApprovalClass bool `json:"approval_class"`
	Approval      bool `json:"approval"`
}

type runData struct {
	Status     string    `json:"status"`
	SpanCount  int       `json:"span_count"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int64     `json:"duration_ms"`
}

// New builds an emitter delivering to subs' targets through policy, whose
// client dials every attempt. baseURL is the public origin joined onto the
// store-provided web path. Call Run to start delivery.
func New(subs Subscriptions, policy *subscription.Policy, baseURL string, log *slog.Logger) *Emitter {
	if policy == nil {
		policy = &subscription.Policy{}
	}
	e := &Emitter{
		subs:    subs,
		policy:  policy,
		baseURL: baseURL,
		log:     log,
		ch:      make(chan envelope, queueCap),
		client:  policy.Client(),
		dropped: make(map[event.Kind]*atomic.Uint64),
	}
	for _, k := range event.RegisteredKinds() {
		e.dropped[k] = new(atomic.Uint64)
	}
	return e
}

// EmitArtifactCreated is the store.CreationEmitter adapter: it forwards the
// creation to Emit as its artifact.created event.
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
	e.Emit(ev.Event())
}

// Emit encodes one event and enqueues it without ever blocking the caller. An
// event encode refuses is logged and dropped, and so is one whose subject has
// no owning workspace: it has nowhere it may go. Past half the queue, events
// other than artifact.created are dropped before enqueueing (EV-8); on a full
// queue any event is dropped with a warning — the doorbell is a hint, not a
// ledger (SPEC-0012 REQ "Bounded Async Delivery with Retry"). The event goes
// only to the owning workspace's subscriptions, never the actor's unless the
// actor's workspace owns the artifact (SPEC-0023 REQ "Events Go Only to the
// Artifact's Workspace"); the lookup runs on a worker, so the caller never
// waits on it.
//
// Governing: SPEC-0016 EV-1 "Unknown kind never emitted", EV-7, EV-8
func (e *Emitter) Emit(ev event.Event) {
	if e == nil {
		return
	}
	env, err := e.seal(ev)
	if err != nil {
		// Never the target URL, and never the body: the kind, the id and why.
		e.log.Error("outboundhook: refusing to emit event",
			"kind", string(ev.Kind), "event_id", env.id, "error", err)
		return
	}
	if !env.owner.Valid() {
		// An artifact always has exactly one owner (SPEC-0023 REQ "Owner
		// Model"); an event without one has nowhere it may go.
		e.drop(env.kind)
		e.log.Warn("outboundhook: event has no owning workspace, dropping it",
			"kind", string(env.kind), "event_id", env.id)
		return
	}
	if env.kind != event.ArtifactCreated && len(e.ch) > priorityDepth {
		// EV-8: keep the rest of the queue for creations.
		e.drop(env.kind)
		e.log.Warn("outboundhook: queue over half full, dropping non-creation event",
			"kind", string(env.kind), "event_id", env.id, "queue_cap", queueCap)
		return
	}
	select {
	case e.ch <- env:
	default:
		e.drop(env.kind)
		e.log.Warn("outboundhook: queue full, dropping event",
			"kind", string(env.kind), "event_id", env.id, "queue_cap", queueCap)
	}
}

// Dropped reports how many events of kind k were encoded and not enqueued:
// events with no owning workspace, non-creation events past half the queue,
// and events that met a full queue. It is the source for SPEC-0016's
// cairn_outbound_events_dropped_total{kind}.
func (e *Emitter) Dropped(k event.Kind) uint64 {
	if e == nil {
		return 0
	}
	if c := e.dropped[k]; c != nil {
		return c.Load()
	}
	return 0
}

func (e *Emitter) drop(k event.Kind) {
	if c := e.dropped[k]; c != nil {
		c.Add(1)
	}
}

// seal assigns an event its id and clock, encodes it, and records the owner
// and filter match that select its targets. The id is set even when encoding
// fails, so the refusal can be logged against it.
func (e *Emitter) seal(ev event.Event) (envelope, error) {
	env := envelope{id: uuid.NewString(), kind: ev.Kind, createdAt: time.Now().UTC()}
	body, err := e.encode(ev, env.id, env.createdAt)
	if err != nil {
		return env, err
	}
	env.body = body
	s := ev.Subject
	env.owner = subscription.Owner{UserID: s.OwnerUserID, TeamID: s.OwnerTeamID}
	env.match = subscription.Match{Kind: string(ev.Kind), ShareType: string(s.ShareType), Tags: s.Tags}
	return env, nil
}

// encode renders the wire body for one event — the exact bytes
// X-Cairn-Signature covers. It takes the event id and clock as arguments so
// golden tests can pin those bytes, which is how a payload change is proven
// additive rather than merely asserted to be. The owner is never encoded.
//
// It refuses, with a sentinel error, a kind outside the registry, an actor
// that is not server-derived, and a payload that does not match the kind. It
// never mutates ev.
//
// Governing: SPEC-0016 EV-1, EV-3 "Payload Shape", EV-4; SPEC-0012 REQ "Event
// Payload"
func (e *Emitter) encode(ev event.Event, eventID string, createdAt time.Time) ([]byte, error) {
	if !ev.Kind.Registered() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, ev.Kind)
	}
	if err := ev.Actor.Check(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidActor, err)
	}
	if err := checkPayload(ev); err != nil {
		return nil, err
	}
	s, a := ev.Subject, ev.Actor
	body := eventBody{
		Source:    "cairn",
		Kind:      string(ev.Kind),
		EventID:   eventID,
		CreatedAt: createdAt,
		Data: eventData{
			ID:         s.PublicID,
			ShareType:  string(s.ShareType),
			Title:      s.Title,
			URL:        e.baseURL + s.WebPath,
			Channel:    string(a.Channel),
			ActorID:    a.ID,
			OnBehalfOf: a.OnBehalfOf,
			Tags:       s.Tags,
			ActorKind:  string(a.Kind),
			Auth:       string(a.Auth),
		},
	}
	if !s.ExpiresAt.IsZero() {
		t := s.ExpiresAt.UTC()
		body.Data.ExpiresAt = &t
	}
	switch ev.Kind {
	case event.ArtifactCreated:
		// The model describes how the artifact was made; no other kind has one.
		body.Data.Model = ev.Model
	case event.CommentCreated:
		c := ev.Comment
		text, cut := truncateBody(c.Body)
		body.Data.Comment = &commentData{
			ID: c.ID, ParentID: c.ParentID, AnchorType: c.AnchorType, AnchorKey: c.AnchorKey,
			Body: text, BodyTruncated: cut,
		}
	case event.ReactionAdded, event.ReactionRemoved:
		r := ev.Reaction
		body.Data.Reaction = &reactionData{
			ID: r.ID, AnchorType: r.AnchorType, AnchorKey: r.AnchorKey, Emoji: r.Emoji,
			ApprovalClass: r.ApprovalClass, Approval: r.Approval,
		}
	case event.RunClosed:
		r := ev.Run
		body.Data.Run = &runData{
			Status: r.Status, SpanCount: r.SpanCount,
			StartedAt: r.StartedAt.UTC(), EndedAt: r.EndedAt.UTC(), DurationMS: r.DurationMS,
		}
	}
	return json.Marshal(body)
}

// checkPayload requires exactly the payload object the kind carries: comment
// for comment.created, reaction for reaction.*, run for run.closed, and none
// for the artifact.* kinds (EV-3). A mismatch means a producer built the event
// for a different kind than it named, so nothing it says can be trusted.
func checkPayload(ev event.Event) error {
	var comment, reaction, run bool
	switch ev.Kind {
	case event.CommentCreated:
		comment = true
	case event.ReactionAdded, event.ReactionRemoved:
		reaction = true
	case event.RunClosed:
		run = true
	}
	if (ev.Comment != nil) != comment || (ev.Reaction != nil) != reaction || (ev.Run != nil) != run {
		return fmt.Errorf("%w: %q", ErrPayloadMismatch, ev.Kind)
	}
	return nil
}

// truncateBody caps a comment body at commentBodyMax bytes, cut on a UTF-8
// boundary, and reports whether it cut. Invalid UTF-8 is replaced first, so
// the string handed to encoding/json is already valid and json.Marshal has
// nothing left to rewrite: the decoded value is exactly these bytes and cannot
// outgrow the cap. (encoding/json would replace each invalid byte with its own
// U+FFFD, three bytes apiece; ToValidUTF8 collapses a run into one, so it must
// run before the length check, not be left to the encoder.) SPEC-0016 EV-3
// "Long comment is truncated in the event only".
func truncateBody(s string) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= commentBodyMax {
		return s, false
	}
	cut := commentBodyMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
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
			e.log.Error("outboundhook: look up subscriptions", "kind", string(env.kind), "event_id", env.id, "error", err)
		}
		return
	}
	if skipped > 0 {
		e.log.Error("outboundhook: subscription secrets do not open under CAIRN_ENCRYPTION_KEY; not delivering to them",
			"kind", string(env.kind), "event_id", env.id, "skipped", skipped)
	}
	for _, t := range targets {
		res := e.deliverOne(ctx, env, t)
		if ctx.Err() != nil {
			// Shutdown interrupted the attempt: that is not the target's fault.
			return
		}
		disabled, err := e.subs.Record(ctx, t.ID, res)
		if err != nil {
			e.log.Error("outboundhook: record delivery", "subscription_id", t.ID, "kind", string(env.kind), "event_id", env.id, "error", err)
		}
		if !res.OK {
			e.log.Warn("outboundhook: delivery failed",
				"subscription_id", t.ID, "kind", string(env.kind), "event_id", env.id, "reason", res.Reason, "status", res.Status)
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
		// The envelope's kind is the kind encode wrote into the body, so the
		// header always matches it (SPEC-0016 EV-8 "Header matches kind").
		req.Header.Set("X-Cairn-Event", string(env.kind))
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
