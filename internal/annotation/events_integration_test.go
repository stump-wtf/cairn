package annotation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// Governing: ADR-0022 (annotation lifecycle events), SPEC-0016 EV-2 "Emission
// Points" ("Duplicate reaction is silent", "Rolled-back write emits nothing"),
// EV-3 "Subject fields resolve the artifact for every kind", EV-5 ("Agent
// thumbs-up is not an approval", "Human thumbs-up with skin tone is an
// approval", "Withdrawn approval is announced"), EV-6 "Agent cannot occupy the
// human's row", "Legacy row is never an approval", and "Concurrent reactions".

// eventSpy records every event the service emits. It is safe for concurrent
// use, as event.Emitter requires.
type eventSpy struct {
	mu  sync.Mutex
	evs []event.Event
}

func (s *eventSpy) Emit(ev event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *eventSpy) all() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.evs...)
}

// take returns the events emitted since the last take and forgets them.
func (s *eventSpy) take() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.evs
	s.evs = nil
	return out
}

// takeOne asserts exactly one event was emitted since the last take, of kind k.
func (s *eventSpy) takeOne(t *testing.T, k event.Kind) event.Event {
	t.Helper()
	evs := s.take()
	if len(evs) != 1 || evs[0].Kind != k {
		t.Fatalf("emitted %s, want exactly one %s", kinds(evs), k)
	}
	return evs[0]
}

// takeNone asserts nothing was emitted since the last take.
func (s *eventSpy) takeNone(t *testing.T, why string) {
	t.Helper()
	if evs := s.take(); len(evs) != 0 {
		t.Fatalf("%s emitted %s, want nothing", why, kinds(evs))
	}
}

func kinds(evs []event.Event) string {
	out := make([]event.Kind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return fmt.Sprint(out)
}

// newEventService is a Service wired to a fresh spy.
func newEventService(pool *pgxpool.Pool) (*Service, *eventSpy) {
	spy := &eventSpy{}
	return New(pool, Options{Registry: sharetype.Default(), Emitter: spy}), spy
}

// assertReaction checks a reaction event's payload and approval pair.
func assertReaction(t *testing.T, ev event.Event, id int64, emoji string, wantClass, wantApproval bool) {
	t.Helper()
	r := ev.Reaction
	if r == nil || ev.Comment != nil || ev.Run != nil {
		t.Fatalf("%s payloads = reaction %v, comment %v, run %v; want only a reaction", ev.Kind, r, ev.Comment, ev.Run)
	}
	if r.ID != id || r.Emoji != emoji || r.AnchorType != string(sharetype.AnchorArtifact) || r.AnchorKey != "{}" {
		t.Errorf("%s reaction = %+v, want id %d emoji %q on the whole artifact", ev.Kind, *r, id, emoji)
	}
	if r.ApprovalClass != wantClass || r.Approval != wantApproval {
		t.Errorf("%s (approval_class, approval) = (%v, %v), want (%v, %v)",
			ev.Kind, r.ApprovalClass, r.Approval, wantClass, wantApproval)
	}
}

// TestReactionEventsApprovalBit walks EV-5 and EV-6 on the service: an agent
// 👍 is stored and announced as in-class but not an approval, the same human's
// browser 👍 is its own row and IS one, a duplicate of either is silent, and
// withdrawing the human's 👍🏽 announces the approval being withdrawn. Every
// event describes the artifact exactly as its creation did (EV-3).
func TestReactionEventsApprovalBit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	artID := insertArtifact(t, pool, "EVTAAAA1", sharetype.KeyMarkdown)
	expires := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE artifacts SET title = 'Work order', tags = ARRAY['handoff','lane:m'], owner_id = 'owner-1', expires_at = $2
		WHERE id = $1`, artID, expires); err != nil {
		t.Fatalf("describe artifact: %v", err)
	}

	// Agent 👍 over MCP: stored, in class, not an approval.
	a, created, err := svc.React(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍", withOBO(agent("alice"), "claude-code/1.0"))
	if err != nil || !created {
		t.Fatalf("agent react: created=%v err=%v", created, err)
	}
	ev := spy.takeOne(t, event.ReactionAdded)
	assertReaction(t, ev, a.ID, "👍", true, false)
	if ev.Actor != withOBO(agent("alice"), "claude-code/1.0") {
		t.Errorf("reaction.added actor = %+v, want the calling agent", ev.Actor)
	}
	want := event.Subject{
		PublicID: "EVTAAAA1", ShareType: sharetype.KeyMarkdown, Title: "Work order",
		WebPath: "/EVTAAAA1", Tags: []string{"handoff", "lane:m"}, ExpiresAt: expires, OwnerID: "owner-1",
	}
	got := ev.Subject
	if got.PublicID != want.PublicID || got.ShareType != want.ShareType || got.Title != want.Title ||
		got.WebPath != want.WebPath || fmt.Sprint(got.Tags) != fmt.Sprint(want.Tags) ||
		!got.ExpiresAt.Equal(want.ExpiresAt) || got.OwnerID != want.OwnerID {
		t.Errorf("subject = %+v, want %+v", got, want)
	}

	// A duplicate of the agent's reaction is silent (EV-2).
	if _, created, err := svc.React(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍", agent("alice")); err != nil || created {
		t.Fatalf("duplicate agent react: created=%v err=%v", created, err)
	}
	spy.takeNone(t, "a duplicate react")

	// The human's own click is a new row and an approval (EV-6 "Agent cannot
	// occupy the human's row").
	h, created, err := svc.React(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍", human("alice"))
	if err != nil || !created {
		t.Fatalf("human react: created=%v err=%v", created, err)
	}
	assertReaction(t, spy.takeOne(t, event.ReactionAdded), h.ID, "👍", true, true)

	// A skin-toned 👍🏽 from a human is an approval too, and withdrawing it
	// announces approval: true so a consumer can revoke what it acted on.
	st, created, err := svc.React(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍🏽", human("bob"))
	if err != nil || !created {
		t.Fatalf("skin-tone react: created=%v err=%v", created, err)
	}
	assertReaction(t, spy.takeOne(t, event.ReactionAdded), st.ID, "👍🏽", true, true)
	removed, err := svc.Unreact(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍🏽", human("bob"))
	if err != nil || !removed {
		t.Fatalf("withdraw: removed=%v err=%v", removed, err)
	}
	ev = spy.takeOne(t, event.ReactionRemoved)
	assertReaction(t, ev, st.ID, "👍🏽", true, true)
	if ev.Actor != human("bob") {
		t.Errorf("reaction.removed actor = %+v, want bob's session", ev.Actor)
	}

	// Removing an absent reaction is a no-op and silent.
	if removed, err := svc.Unreact(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍🏽", human("bob")); err != nil || removed {
		t.Fatalf("repeat withdraw: removed=%v err=%v", removed, err)
	}
	spy.takeNone(t, "removing an absent reaction")

	// The agent's un-react takes only the agent's row, announced as the
	// non-approval it was; alice's approval stands.
	if removed, err := svc.Unreact(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "👍", agent("alice")); err != nil || !removed {
		t.Fatalf("agent unreact: removed=%v err=%v", removed, err)
	}
	assertReaction(t, spy.takeOne(t, event.ReactionRemoved), a.ID, "👍", true, false)
	if rows := reactionRows(t, pool, artID, "👍", "alice"); len(rows) != 1 || !hasKey(rows, "human") {
		t.Fatalf("alice's rows after her agent's unreact = %v, want only the human row", rows)
	}

	// A non-approval emoji is out of class for everyone.
	p, _, err := svc.React(ctx, "EVTAAAA1", sharetype.AnchorArtifact, nil, "🎉", human("alice"))
	if err != nil {
		t.Fatalf("party react: %v", err)
	}
	assertReaction(t, spy.takeOne(t, event.ReactionAdded), p.ID, "🎉", false, false)
}

// TestUnreactByIDEvents: a delete by id announces the deleted row with the
// emoji and anchor it was stored with, classified by its stored kind. A refused
// delete (the other kind's row: 403) and a missing row (404) announce nothing.
func TestUnreactByIDEvents(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	insertArtifact(t, pool, "EVTAAAA2", sharetype.KeyMarkdown)

	h, _, err := svc.React(ctx, "EVTAAAA2", sharetype.AnchorArtifact, nil, "✅", human("alice"))
	if err != nil {
		t.Fatalf("human react: %v", err)
	}
	spy.take()

	if err := svc.UnreactByID(ctx, "EVTAAAA2", h.ID, agent("alice")); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("agent delete of the human's row: err = %v, want forbidden", err)
	}
	spy.takeNone(t, "a refused delete by id")
	if err := svc.UnreactByID(ctx, "EVTAAAA2", h.ID+1000, human("alice")); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("delete of a missing row: err = %v, want not found", err)
	}
	spy.takeNone(t, "a delete of a missing row")

	if err := svc.UnreactByID(ctx, "EVTAAAA2", h.ID, human("alice")); err != nil {
		t.Fatalf("human delete by id: %v", err)
	}
	ev := spy.takeOne(t, event.ReactionRemoved)
	assertReaction(t, ev, h.ID, "✅", true, true)
	if ev.Actor != human("alice") || ev.Subject.PublicID != "EVTAAAA2" {
		t.Errorf("reaction.removed actor %+v subject %q, want alice's session on EVTAAAA2", ev.Actor, ev.Subject.PublicID)
	}
}

// TestLegacyRemovalIsNeverAnApproval is EV-6 "Legacy row is never an
// approval": a pre-kind 👍 removed by its human, by either route, announces
// approval: false. When the human's own row and a legacy row go in one
// un-react, each is announced separately with its own bit.
func TestLegacyRemovalIsNeverAnApproval(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	artID := insertArtifact(t, pool, "EVTAAAA3", sharetype.KeyMarkdown)

	byID := insertLegacyReaction(t, pool, artID, "👍", "alice")
	if err := svc.UnreactByID(ctx, "EVTAAAA3", byID, human("alice")); err != nil {
		t.Fatalf("delete legacy row by id: %v", err)
	}
	assertReaction(t, spy.takeOne(t, event.ReactionRemoved), byID, "👍", true, false)

	legacy := insertLegacyReaction(t, pool, artID, "👍", "alice")
	own, _, err := svc.React(ctx, "EVTAAAA3", sharetype.AnchorArtifact, nil, "👍", human("alice"))
	if err != nil {
		t.Fatalf("human react beside the legacy row: %v", err)
	}
	spy.take()
	if removed, err := svc.Unreact(ctx, "EVTAAAA3", sharetype.AnchorArtifact, nil, "👍", human("alice")); err != nil || !removed {
		t.Fatalf("unreact: removed=%v err=%v", removed, err)
	}
	evs := spy.take()
	if len(evs) != 2 {
		t.Fatalf("unreact of a legacy and an own row emitted %s, want two reaction.removed", kinds(evs))
	}
	approvals := map[int64]bool{}
	for _, ev := range evs {
		if ev.Kind != event.ReactionRemoved || ev.Reaction == nil {
			t.Fatalf("emitted %+v, want reaction.removed", ev)
		}
		approvals[ev.Reaction.ID] = ev.Reaction.Approval
	}
	if a, ok := approvals[legacy]; !ok || a {
		t.Errorf("legacy row %d: announced %v approval %v, want announced with approval false", legacy, ok, a)
	}
	if a, ok := approvals[own.ID]; !ok || !a {
		t.Errorf("own row %d: announced %v approval %v, want announced with approval true", own.ID, ok, a)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestCommentCreatedEvent: a committed comment announces its id, thread
// parent, anchor and full body; the encoder, not the service, truncates. The
// actor is the derived one, whatever it claims on_behalf_of.
func TestCommentCreatedEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	insertArtifact(t, pool, "EVTAAAA4", sharetype.KeyMarkdown)

	body := strings.Repeat("x", 10<<10) // 10 KiB: longer than the wire cap

	actor := withOBO(agent("alice"), "claude-code/1.0")
	root, err := svc.AddComment(ctx, "EVTAAAA4", CommentInput{AnchorType: sharetype.AnchorArtifact, Actor: actor, Body: body})
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}
	ev := spy.takeOne(t, event.CommentCreated)
	c := ev.Comment
	if c == nil || ev.Reaction != nil || ev.Run != nil {
		t.Fatalf("comment.created payloads = comment %v, reaction %v, run %v", c, ev.Reaction, ev.Run)
	}
	if c.ID != root.ID || c.ParentID != nil || c.AnchorType != "artifact" || c.AnchorKey != "{}" || c.Body != body {
		t.Errorf("comment.created comment = id %d parent %v anchor %s/%s body %d bytes; want id %d, root, artifact/{}, %d bytes",
			c.ID, c.ParentID, c.AnchorType, c.AnchorKey, len(c.Body), root.ID, len(body))
	}
	if ev.Actor != actor || ev.Subject.PublicID != "EVTAAAA4" {
		t.Errorf("comment.created actor %+v subject %q, want %+v on EVTAAAA4", ev.Actor, ev.Subject.PublicID, actor)
	}

	reply, err := svc.AddComment(ctx, "EVTAAAA4", CommentInput{ParentID: &root.ID, Actor: human("bob"), Body: "agreed"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	ev = spy.takeOne(t, event.CommentCreated)
	if ev.Comment.ID != reply.ID || ev.Comment.ParentID == nil || *ev.Comment.ParentID != root.ID ||
		ev.Actor.Kind != event.KindHuman {
		t.Errorf("reply comment.created = %+v by %+v, want id %d under %d by a human", *ev.Comment, ev.Actor, reply.ID, root.ID)
	}

	// Refusals inside the transaction announce nothing.
	missing := root.ID + 1000
	if _, err := svc.AddComment(ctx, "EVTAAAA4", CommentInput{ParentID: &missing, Actor: human("bob"), Body: "lost"}); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("reply to a missing parent: err = %v, want ErrParentNotFound", err)
	}
	spy.takeNone(t, "a refused reply")
}

// failArtifactUpdates makes every UPDATE of artifacts raise, so a write whose
// row INSERT already succeeded fails at its counter bump and its transaction
// rolls back. The trigger lives in the test's private schema.
func failArtifactUpdates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_artifact_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected counter failure'; END $$`); err != nil {
		t.Fatalf("create failing function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER fail_artifact_update BEFORE UPDATE ON artifacts
		FOR EACH ROW EXECUTE FUNCTION fail_artifact_update()`); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
}

// TestRolledBackWritesEmitNothing is EV-2 "Rolled-back write emits nothing":
// each write gets past validation and its row write, then fails at the
// counter bump inside the same transaction. Nothing is stored and nothing is
// announced.
func TestRolledBackWritesEmitNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	artID := insertArtifact(t, pool, "EVTAAAA5", sharetype.KeyMarkdown)

	// Positive control: before the fault, the same writes do announce.
	h, _, err := svc.React(ctx, "EVTAAAA5", sharetype.AnchorArtifact, nil, "👍", human("alice"))
	if err != nil {
		t.Fatalf("control react: %v", err)
	}
	spy.takeOne(t, event.ReactionAdded)

	failArtifactUpdates(t, pool)

	if _, err := svc.AddComment(ctx, "EVTAAAA5", CommentInput{AnchorType: sharetype.AnchorArtifact, Actor: human("alice"), Body: "hi"}); err == nil {
		t.Fatal("comment with a failing counter bump succeeded, want an error")
	}
	spy.takeNone(t, "a rolled-back comment")
	if _, _, err := svc.React(ctx, "EVTAAAA5", sharetype.AnchorArtifact, nil, "🎉", human("alice")); err == nil {
		t.Fatal("react with a failing counter bump succeeded, want an error")
	}
	spy.takeNone(t, "a rolled-back react")
	if _, err := svc.Unreact(ctx, "EVTAAAA5", sharetype.AnchorArtifact, nil, "👍", human("alice")); err == nil {
		t.Fatal("unreact with a failing counter bump succeeded, want an error")
	}
	spy.takeNone(t, "a rolled-back unreact")
	if err := svc.UnreactByID(ctx, "EVTAAAA5", h.ID, human("alice")); err == nil {
		t.Fatal("unreact by id with a failing counter bump succeeded, want an error")
	}
	spy.takeNone(t, "a rolled-back unreact by id")

	var comments, reactions int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM comments WHERE artifact_id = $1),
		       (SELECT count(*) FROM reactions WHERE artifact_id = $1)`, artID).Scan(&comments, &reactions); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if comments != 0 || reactions != 1 {
		t.Fatalf("after rollbacks: %d comments, %d reactions; want 0 and the control's 1", comments, reactions)
	}
}

// TestConcurrentReactionsEmitOncePerInsert is SPEC-0016 "Concurrent
// reactions": 50 concurrent reacts — 25 actors, each racing itself — insert
// 25 rows and emit exactly 25 reaction.added, one per inserted row id. Run
// under -race in CI.
func TestConcurrentReactionsEmitOncePerInsert(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc, spy := newEventService(pool)
	artID := insertArtifact(t, pool, "EVTAAAA6", sharetype.KeyMarkdown)

	const actors, perActor = 25, 2
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created = map[int64]bool{}
	)
	for i := 0; i < actors*perActor; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			a := human(fmt.Sprintf("actor-%d", n%actors))
			r, isNew, err := svc.React(ctx, "EVTAAAA6", sharetype.AnchorArtifact, nil, "👍", a)
			if err != nil {
				t.Errorf("react: %v", err)
				return
			}
			if isNew {
				mu.Lock()
				created[r.ID] = true
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	evs := spy.all()
	if len(created) != actors || len(evs) != actors {
		t.Fatalf("%d inserts and %d events from %d reacts, want %d of each", len(created), len(evs), actors*perActor, actors)
	}
	seen := map[int64]bool{}
	for _, ev := range evs {
		if ev.Kind != event.ReactionAdded || ev.Reaction == nil || !created[ev.Reaction.ID] || seen[ev.Reaction.ID] {
			t.Fatalf("event %+v is not one reaction.added per inserted row", ev)
		}
		seen[ev.Reaction.ID] = true
	}
	assertCountsMatchAggregates(t, pool, artID)
}
