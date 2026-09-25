package annotation

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// Per-kind ownership of reactions and comments. alice's agent (any bearer
// credential) and alice in her browser share the actor_id "alice"; only the
// stored actor_kind keeps their annotations apart.
//
// Governing: ADR-0022, SPEC-0016 EV-6 "Per-Kind Reaction and Comment
// Ownership"; SPEC-0006 REQ "Idempotent Reactions" ("Duplicate reaction",
// "Un-react", now per kind).

// reactionRows returns the kinds and on_behalf_of values of one actor's rows
// for (artifact, emoji), keyed by kind.
func reactionRows(t *testing.T, pool *pgxpool.Pool, artID int64, emoji, actorID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT actor_kind, on_behalf_of FROM reactions
		WHERE artifact_id = $1 AND emoji = $2 AND actor_id = $3`, artID, emoji, actorID)
	if err != nil {
		t.Fatalf("read reactions: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, obo string
		if err := rows.Scan(&kind, &obo); err != nil {
			t.Fatalf("scan reaction: %v", err)
		}
		out[kind] = obo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate reactions: %v", err)
	}
	return out
}

// insertLegacyReaction writes a row the way the code before per-kind storage
// did: no kind, no on_behalf_of.
func insertLegacyReaction(t *testing.T, pool *pgxpool.Pool, artID int64, emoji, actorID string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id)
		VALUES ($1, 'artifact', '{}', '{}', $2, $3) RETURNING id`, artID, emoji, actorID).Scan(&id); err != nil {
		t.Fatalf("insert legacy reaction: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE artifacts SET reaction_count = reaction_count + 1 WHERE id = $1`, artID); err != nil {
		t.Fatalf("bump legacy count: %v", err)
	}
	return id
}

func tallyFor(t *testing.T, svc *Service, publicID, emoji string, viewer Viewer) Tally {
	t.Helper()
	tallies, err := svc.ReactionTallies(context.Background(), publicID, viewer)
	if err != nil {
		t.Fatalf("tallies: %v", err)
	}
	for _, tl := range tallies {
		if tl.Emoji == emoji {
			return tl
		}
	}
	return Tally{Emoji: emoji}
}

// TestAgentCannotOccupyHumansRow is EV-6 "Agent cannot occupy the human's
// row": the agent reacting first does not make the human's later click a
// no-op. Each kind is still idempotent on its own, and provenance is stored
// per row: the agent's on_behalf_of, and none for the browser.
func TestAgentCannotOccupyHumansRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	artID := insertArtifact(t, pool, "OWNAAAA1", sharetype.KeyMarkdown)

	a, created, err := svc.React(ctx, "OWNAAAA1", sharetype.AnchorArtifact, nil, "👍", withOBO(agent("alice"), "claude-code/1.0"))
	if err != nil || !created {
		t.Fatalf("agent react: created=%v err=%v", created, err)
	}
	if a.ActorKind != event.KindAgent || a.OnBehalfOf != "claude-code/1.0" {
		t.Fatalf("agent reaction = %+v, want kind agent on behalf of claude-code/1.0", a)
	}
	h, created, err := svc.React(ctx, "OWNAAAA1", sharetype.AnchorArtifact, nil, "👍", human("alice"))
	if err != nil || !created {
		t.Fatalf("human react after the agent: created=%v err=%v, want a new row", created, err)
	}
	if h.ID == a.ID || h.ActorKind != event.KindHuman || h.OnBehalfOf != "" {
		t.Fatalf("human reaction = %+v (agent row %d), want its own human row with no on_behalf_of", h, a.ID)
	}

	// Per-kind idempotency: a repeat of either is the existing row, and the
	// agent's repeat keeps the provenance it was first recorded with.
	again, created, err := svc.React(ctx, "OWNAAAA1", sharetype.AnchorArtifact, nil, "👍", withOBO(agent("alice"), "other-harness"))
	if err != nil || created || again.ID != a.ID || again.OnBehalfOf != "claude-code/1.0" {
		t.Fatalf("repeat agent react = %+v created=%v err=%v, want the first row unchanged", again, created, err)
	}
	if again, created, err := svc.React(ctx, "OWNAAAA1", sharetype.AnchorArtifact, nil, "👍", human("alice")); err != nil || created || again.ID != h.ID {
		t.Fatalf("repeat human react = %+v created=%v err=%v, want row %d", again, created, err, h.ID)
	}

	if got := reactionRows(t, pool, artID, "👍", "alice"); len(got) != 2 || got["agent"] != "claude-code/1.0" || got["human"] != "" {
		t.Fatalf("stored rows = %v, want one agent row (on behalf of claude-code/1.0) and one human row", got)
	}
	tl := tallyFor(t, svc, "OWNAAAA1", "👍", Viewer{})
	if tl.Count != 2 || tl.HumanCount != 1 || tl.AgentCount != 1 || tl.Reacted {
		t.Fatalf("anonymous tally = %+v, want 2 (1 human, 1 agent), not reacted", tl)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestAgentCannotWithdrawHumansReaction is EV-6 "Agent cannot withdraw the
// human's approval", by value and by id, in both directions.
func TestAgentCannotWithdrawHumansReaction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	artID := insertArtifact(t, pool, "OWNAAAA2", sharetype.KeyMarkdown)

	h, _, err := svc.React(ctx, "OWNAAAA2", sharetype.AnchorArtifact, nil, "👍", human("alice"))
	if err != nil {
		t.Fatalf("human react: %v", err)
	}

	// By value: the agent has no row, so nothing is removed.
	removed, err := svc.Unreact(ctx, "OWNAAAA2", sharetype.AnchorArtifact, nil, "👍", agent("alice"))
	if err != nil || removed {
		t.Fatalf("agent un-react of the human's 👍: removed=%v err=%v, want nothing removed", removed, err)
	}
	// "Agent cannot remove by id": forbidden, and the row stands.
	if err := svc.UnreactByID(ctx, "OWNAAAA2", h.ID, agent("alice")); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("agent un-react by id = %v, want ErrForbidden", err)
	}
	if got := reactionRows(t, pool, artID, "👍", "alice"); len(got) != 1 {
		t.Fatalf("rows after the agent's attempts = %v, want the human row alone", got)
	}

	// With both rows present, the agent's un-react removes only its own.
	if _, _, err := svc.React(ctx, "OWNAAAA2", sharetype.AnchorArtifact, nil, "👍", agent("alice")); err != nil {
		t.Fatalf("agent react: %v", err)
	}
	if removed, err := svc.Unreact(ctx, "OWNAAAA2", sharetype.AnchorArtifact, nil, "👍", agent("alice")); err != nil || !removed {
		t.Fatalf("agent un-react of its own row: removed=%v err=%v", removed, err)
	}
	if got := reactionRows(t, pool, artID, "👍", "alice"); len(got) != 1 || !hasKey(got, "human") {
		t.Fatalf("rows after the agent's un-react = %v, want the human row alone", got)
	}
	if tl := tallyFor(t, svc, "OWNAAAA2", "👍", Viewer{ID: "alice", Kind: event.KindAgent}); tl.Reacted {
		t.Fatalf("agent's tally = %+v, want reacted=false: the row is the human's", tl)
	}
	if tl := tallyFor(t, svc, "OWNAAAA2", "👍", Viewer{ID: "alice", Kind: event.KindHuman}); !tl.Reacted || tl.HumanCount != 1 {
		t.Fatalf("human's tally = %+v, want reacted with one human row", tl)
	}

	// The reverse holds too: the human cannot remove her agent's row by id.
	ag, _, err := svc.React(ctx, "OWNAAAA2", sharetype.AnchorArtifact, nil, "👍", agent("alice"))
	if err != nil {
		t.Fatalf("agent re-react: %v", err)
	}
	if err := svc.UnreactByID(ctx, "OWNAAAA2", ag.ID, human("alice")); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("human un-react of the agent row by id = %v, want ErrForbidden", err)
	}

	// Positive controls: each kind removes its own row by id.
	if err := svc.UnreactByID(ctx, "OWNAAAA2", h.ID, human("alice")); err != nil {
		t.Fatalf("human un-react by id: %v", err)
	}
	if err := svc.UnreactByID(ctx, "OWNAAAA2", ag.ID, agent("alice")); err != nil {
		t.Fatalf("agent un-react by id: %v", err)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 0 {
		t.Fatalf("reaction_count = %d, want 0", r)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestLegacyReactionIsNeverHuman is EV-6 "Legacy row is never an approval": a
// row written before kinds were stored counts as neither kind, is removable
// by its own actor_id as either kind, and by nobody else.
func TestLegacyReactionIsNeverHuman(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	artID := insertArtifact(t, pool, "OWNAAAA3", sharetype.KeyMarkdown)

	legacy := insertLegacyReaction(t, pool, artID, "👍", "alice")
	tl := tallyFor(t, svc, "OWNAAAA3", "👍", Viewer{ID: "alice", Kind: event.KindHuman})
	if tl.Count != 1 || tl.HumanCount != 0 || tl.AgentCount != 0 {
		t.Fatalf("legacy tally = %+v, want count 1 of neither kind", tl)
	}
	if !tl.Reacted {
		t.Fatalf("legacy tally = %+v, want reacted for its own actor (she can remove it)", tl)
	}
	if tl := tallyFor(t, svc, "OWNAAAA3", "👍", Viewer{ID: "bob", Kind: event.KindHuman}); tl.Reacted {
		t.Fatalf("bob's tally = %+v, want reacted=false", tl)
	}

	// Another actor may not remove it, whatever its kind.
	if err := svc.UnreactByID(ctx, "OWNAAAA3", legacy, human("bob")); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("bob un-react by id = %v, want ErrForbidden", err)
	}
	if removed, err := svc.Unreact(ctx, "OWNAAAA3", sharetype.AnchorArtifact, nil, "👍", human("bob")); err != nil || removed {
		t.Fatalf("bob un-react: removed=%v err=%v, want nothing removed", removed, err)
	}
	// Its own actor removes it as either kind: here the agent credential.
	if removed, err := svc.Unreact(ctx, "OWNAAAA3", sharetype.AnchorArtifact, nil, "👍", agent("alice")); err != nil || !removed {
		t.Fatalf("alice's agent un-react of the legacy row: removed=%v err=%v", removed, err)
	}

	// By id, as the human; and a legacy row plus a same-kind row both go in
	// one toggle-off, with the rollup following the rows.
	legacy = insertLegacyReaction(t, pool, artID, "🎉", "alice")
	if err := svc.UnreactByID(ctx, "OWNAAAA3", legacy, human("alice")); err != nil {
		t.Fatalf("human un-react of her legacy row by id: %v", err)
	}
	insertLegacyReaction(t, pool, artID, "🔥", "alice")
	if _, _, err := svc.React(ctx, "OWNAAAA3", sharetype.AnchorArtifact, nil, "🔥", human("alice")); err != nil {
		t.Fatalf("human react beside a legacy row: %v", err)
	}
	if removed, err := svc.Unreact(ctx, "OWNAAAA3", sharetype.AnchorArtifact, nil, "🔥", human("alice")); err != nil || !removed {
		t.Fatalf("human un-react: removed=%v err=%v", removed, err)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 0 {
		t.Fatalf("reaction_count = %d, want 0", r)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestCommentOwnershipPerKind stores the derived kind on comments and holds
// edit and delete to the author's kind, with the same legacy grace.
func TestCommentOwnershipPerKind(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	artID := insertArtifact(t, pool, "OWNAAAA4", sharetype.KeyMarkdown)

	c, err := svc.AddComment(ctx, "OWNAAAA4", CommentInput{
		AnchorType: sharetype.AnchorArtifact, Actor: human("alice"), Body: "approved",
	})
	if err != nil {
		t.Fatalf("human comment: %v", err)
	}
	if c.ActorKind != event.KindHuman {
		t.Fatalf("comment kind = %q, want human", c.ActorKind)
	}
	if err := svc.EditComment(ctx, "OWNAAAA4", c.ID, agent("alice"), "rejected"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("agent edit of the human's comment = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteComment(ctx, "OWNAAAA4", c.ID, agent("alice")); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("agent delete of the human's comment = %v, want ErrForbidden", err)
	}

	var legacyID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, actor_id, body)
		VALUES ($1, 'artifact', '{}', '{}', 'alice', 'old') RETURNING id`, artID).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy comment: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE artifacts SET comment_count = comment_count + 1 WHERE id = $1`, artID); err != nil {
		t.Fatalf("bump legacy count: %v", err)
	}

	list, err := svc.ListComments(ctx, "OWNAAAA4")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	kinds := map[int64]event.ActorKind{}
	for _, lc := range list {
		kinds[lc.ID] = lc.ActorKind
	}
	if kinds[c.ID] != event.KindHuman || kinds[legacyID] != "" {
		t.Fatalf("listed kinds = %v, want human for %d and '' for the legacy %d", kinds, c.ID, legacyID)
	}

	if err := svc.EditComment(ctx, "OWNAAAA4", legacyID, human("bob"), "hijack"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("bob edit of the legacy comment = %v, want ErrForbidden", err)
	}
	if err := svc.EditComment(ctx, "OWNAAAA4", legacyID, agent("alice"), "old, edited"); err != nil {
		t.Fatalf("alice's agent edit of her legacy comment: %v", err)
	}
	// Positive controls: the author, as her own kind.
	if err := svc.EditComment(ctx, "OWNAAAA4", c.ID, human("alice"), "approved, edited"); err != nil {
		t.Fatalf("human edit: %v", err)
	}
	if err := svc.DeleteComment(ctx, "OWNAAAA4", c.ID, human("alice")); err != nil {
		t.Fatalf("human delete: %v", err)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
