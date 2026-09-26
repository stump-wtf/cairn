package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// artifactCounts reads the denormalized rollups straight off the artifact row.
func artifactCounts(t *testing.T, pool *pgxpool.Pool, artID int64) (reactions, comments, pins int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT reaction_count, comment_count, pin_count FROM artifacts WHERE id = $1`, artID,
	).Scan(&reactions, &comments, &pins); err != nil {
		t.Fatalf("read artifact counts: %v", err)
	}
	return reactions, comments, pins
}

// assertCountsMatchAggregates asserts the ADR-0006 count-consistency
// confirmation: the denormalized counters equal a fresh aggregate over the
// annotation tables (live comments only; pin = image-region rows of both
// kinds).
func assertCountsMatchAggregates(t *testing.T, pool *pgxpool.Pool, artID int64) {
	t.Helper()
	ctx := context.Background()
	var wantReactions, wantComments, wantPins int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reactions WHERE artifact_id = $1`, artID).Scan(&wantReactions); err != nil {
		t.Fatalf("aggregate reactions: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE artifact_id = $1 AND deleted_at IS NULL`, artID).Scan(&wantComments); err != nil {
		t.Fatalf("aggregate comments: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reactions WHERE artifact_id = $1 AND anchor_type = 'image_region')
		     + (SELECT count(*) FROM comments  WHERE artifact_id = $1 AND anchor_type = 'image_region' AND deleted_at IS NULL)`,
		artID).Scan(&wantPins); err != nil {
		t.Fatalf("aggregate pins: %v", err)
	}
	gotReactions, gotComments, gotPins := artifactCounts(t, pool, artID)
	if gotReactions != wantReactions || gotComments != wantComments || gotPins != wantPins {
		t.Fatalf("rollups (r=%d c=%d p=%d) diverge from fresh aggregates (r=%d c=%d p=%d)",
			gotReactions, gotComments, gotPins, wantReactions, wantComments, wantPins)
	}
}

// TestServiceReactToggle exercises the reaction toggle end to end through the
// core service: duplicate react is a no-op returning the same row, un-react
// removes exactly that row, and the artifact rollup tracks every step in the
// same transaction (SPEC-0006 REQ "Idempotent Reactions", REQ "Count
// Aggregation").
func TestServiceReactToggle(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	artID := insertArtifact(t, pool, "SVCAAAA1", sharetype.KeyCode)

	ref := json.RawMessage(`{"start":40,"end":47}`)
	r1, created, err := svc.React(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "🔥", "u1")
	if err != nil || !created {
		t.Fatalf("first react: created=%v err=%v", created, err)
	}
	// Same anchor in a different key order is the same reaction (canonical key).
	r2, created, err := svc.React(ctx, "SVCAAAA1", sharetype.AnchorCodeRange,
		json.RawMessage(`{ "end": 47, "start": 40 }`), "🔥", "u1")
	if err != nil || created {
		t.Fatalf("duplicate react: created=%v err=%v, want no-op", created, err)
	}
	if r2.ID != r1.ID {
		t.Fatalf("duplicate react returned row %d, want existing row %d", r2.ID, r1.ID)
	}
	if r, c, p := artifactCounts(t, pool, artID); r != 1 || c != 0 || p != 0 {
		t.Fatalf("counts after duplicate react = (%d,%d,%d), want (1,0,0)", r, c, p)
	}

	// A different emoji and a different actor are distinct reactions.
	if _, created, err = svc.React(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "🎉", "u1"); err != nil || !created {
		t.Fatalf("second emoji react: created=%v err=%v", created, err)
	}
	if _, created, err = svc.React(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "🔥", "u2"); err != nil || !created {
		t.Fatalf("second actor react: created=%v err=%v", created, err)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 3 {
		t.Fatalf("reaction_count = %d, want 3", r)
	}

	// Un-react removes exactly u1's 🔥 and decrements the rollup.
	removed, err := svc.Unreact(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "🔥", "u1")
	if err != nil || !removed {
		t.Fatalf("unreact: removed=%v err=%v", removed, err)
	}
	// Removing it again is a toggle no-op.
	removed, err = svc.Unreact(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "🔥", "u1")
	if err != nil || removed {
		t.Fatalf("second unreact: removed=%v err=%v, want no-op", removed, err)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 2 {
		t.Fatalf("reaction_count after unreact = %d, want 2", r)
	}
	assertCountsMatchAggregates(t, pool, artID)

	// Tallies group per (anchor, emoji) with the caller's "did I react" flag.
	tallies, err := svc.ReactionTallies(ctx, "SVCAAAA1", "u2")
	if err != nil {
		t.Fatalf("tallies: %v", err)
	}
	if len(tallies) != 2 {
		t.Fatalf("got %d tallies, want 2: %+v", len(tallies), tallies)
	}
	for _, tl := range tallies {
		switch tl.Emoji {
		case "🔥":
			if tl.Count != 1 || !tl.Reacted {
				t.Errorf("🔥 tally = %+v, want count 1 reacted by u2", tl)
			}
		case "🎉":
			if tl.Count != 1 || tl.Reacted {
				t.Errorf("🎉 tally = %+v, want count 1 not reacted by u2", tl)
			}
		default:
			t.Errorf("unexpected tally emoji %q", tl.Emoji)
		}
	}

	// Registry gate: reacting on an anchor outside the type's capability set
	// persists nothing.
	if _, _, err := svc.React(ctx, "SVCAAAA1", sharetype.AnchorImageRegion,
		json.RawMessage(`{"x":0.5,"y":0.5}`), "🔥", "u1"); !errors.Is(err, ErrAnchorNotAllowed) {
		t.Fatalf("image_region react on code artifact = %v, want ErrAnchorNotAllowed", err)
	}
	// Invalid emoji persists nothing.
	if _, _, err := svc.React(ctx, "SVCAAAA1", sharetype.AnchorCodeRange, ref, "not an emoji", "u1"); !errors.Is(err, ErrEmojiInvalid) {
		t.Fatalf("invalid emoji react = %v, want ErrEmojiInvalid", err)
	}
	assertCountsMatchAggregates(t, pool, artID)

	// Unknown artifact is a uniform not-found.
	if _, _, err := svc.React(ctx, "NOPENOPE", sharetype.AnchorArtifact, nil, "🔥", "u1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("react on unknown artifact = %v, want ErrNotFound", err)
	}
}

// TestServiceReactConcurrentIdempotent hammers React with identical concurrent
// requests (run under -race in CI): the unique constraint must collapse them
// to one row, exactly one caller may observe created=true, and the rollup must
// be bumped exactly once (ADR-0006 "idempotency by unique constraint, not
// read-modify-write").
func TestServiceReactConcurrentIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	artID := insertArtifact(t, pool, "SVCAAAA2", sharetype.KeyMarkdown)

	const goroutines = 16
	ref := json.RawMessage(`{"block_id":"b_3f2a"}`)

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		createdCount int
		errCount     int
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := svc.React(ctx, "SVCAAAA2", sharetype.AnchorMarkdownBlock, ref, "🔥", "u1")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errCount++
				t.Errorf("concurrent react: %v", err)
				return
			}
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()

	if errCount != 0 {
		t.Fatalf("%d concurrent reacts errored", errCount)
	}
	if createdCount != 1 {
		t.Fatalf("created=true observed %d times, want exactly 1", createdCount)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions WHERE artifact_id = $1`, artID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d concurrent identical reacts persisted %d rows, want 1", goroutines, rows)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 1 {
		t.Fatalf("reaction_count = %d after concurrent duplicate reacts, want 1", r)
	}

	// Distinct actors racing the same anchor all land, and the rollup matches.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			actor := fmt.Sprintf("actor-%d", n)
			if _, _, err := svc.React(ctx, "SVCAAAA2", sharetype.AnchorMarkdownBlock, ref, "👀", actor); err != nil {
				t.Errorf("actor %s react: %v", actor, err)
			}
		}(i)
	}
	wg.Wait()
	assertCountsMatchAggregates(t, pool, artID)
}

// TestServiceUnreactByID covers the REST delete-by-id shape: author-only, with
// forbidden vs not-found distinguished, and the rollup decremented in the same
// transaction.
func TestServiceUnreactByID(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	artID := insertArtifact(t, pool, "SVCAAAA3", sharetype.KeyWebhook)

	r, created, err := svc.React(ctx, "SVCAAAA3", sharetype.AnchorWebhookRequest,
		json.RawMessage(`{"request_id":"req_7Kx9"}`), "👀", "u1")
	if err != nil || !created {
		t.Fatalf("react: created=%v err=%v", created, err)
	}

	// Another actor may not remove it.
	if err := svc.UnreactByID(ctx, "SVCAAAA3", r.ID, "u2"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("foreign unreact = %v, want ErrForbidden", err)
	}
	// The author may.
	if err := svc.UnreactByID(ctx, "SVCAAAA3", r.ID, "u1"); err != nil {
		t.Fatalf("unreact by id: %v", err)
	}
	if err := svc.UnreactByID(ctx, "SVCAAAA3", r.ID, "u1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("second unreact by id = %v, want ErrNotFound", err)
	}
	if r, _, _ := artifactCounts(t, pool, artID); r != 0 {
		t.Fatalf("reaction_count = %d, want 0", r)
	}
}

// TestServiceCommentThreading drives the comment path end to end: roots and
// one-level replies (anchor inherited or asserted), thread-ordered listing,
// author-only edit/soft-delete with the tombstone preserving replies, and the
// registry-gated refusals (SPEC-0006 REQ "Threaded Comments", REQ "Webhook
// Reaction-Only Asymmetry").
func TestServiceCommentThreading(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	artID := insertArtifact(t, pool, "SVCAAAA4", sharetype.KeyMarkdown)

	sel := json.RawMessage(`{"quote":"ship it","start":10,"end":17}`)
	root1, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		AnchorType: sharetype.AnchorTextSelection, AnchorRef: sel,
		ActorID: "u1", Body: "root one",
	})
	if err != nil {
		t.Fatalf("root1: %v", err)
	}
	root2, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		AnchorType: sharetype.AnchorArtifact,
		ActorID:    "u1", OnBehalfOf: "agent-7", Body: "root two",
	})
	if err != nil {
		t.Fatalf("root2: %v", err)
	}

	// A reply with no anchor inherits its root's (SPEC-0006 "a reply's anchor
	// MUST match its root's anchor").
	reply1, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		ParentID: &root1.ID, ActorID: "u2", Body: "reply to one",
	})
	if err != nil {
		t.Fatalf("reply1: %v", err)
	}
	if reply1.Anchor.Type != root1.Anchor.Type || reply1.Anchor.Key != root1.Anchor.Key {
		t.Fatalf("reply anchor %s/%s differs from root %s/%s",
			reply1.Anchor.Type, reply1.Anchor.Key, root1.Anchor.Type, root1.Anchor.Key)
	}
	// A reply asserting a different anchor is refused.
	if _, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		AnchorType: sharetype.AnchorArtifact,
		ParentID:   &root1.ID, ActorID: "u2", Body: "mismatched",
	}); !errors.Is(err, ErrAnchorMismatch) {
		t.Fatalf("mismatched reply = %v, want ErrAnchorMismatch", err)
	}
	// A reply to a reply exceeds the one-level thread depth.
	if _, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		ParentID: &reply1.ID, ActorID: "u1", Body: "too deep",
	}); !errors.Is(err, ErrThreadTooDeep) {
		t.Fatalf("reply-to-reply = %v, want ErrThreadTooDeep", err)
	}
	// A reply to a missing parent is refused.
	missing := int64(999999)
	if _, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		ParentID: &missing, ActorID: "u1", Body: "orphan",
	}); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("orphan reply = %v, want ErrParentNotFound", err)
	}
	// Comments on a webhook artifact are structurally refused (registry data).
	insertArtifact(t, pool, "SVCHOOK1", sharetype.KeyWebhook)
	if _, err := svc.AddComment(ctx, "SVCHOOK1", CommentInput{
		AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "nope",
	}); !errors.Is(err, ErrNotCommentable) {
		t.Fatalf("webhook comment = %v, want ErrNotCommentable", err)
	}
	// An empty body is refused.
	if _, err := svc.AddComment(ctx, "SVCAAAA4", CommentInput{
		AnchorType: sharetype.AnchorArtifact, ActorID: "u1",
	}); !errors.Is(err, ErrBodyInvalid) {
		t.Fatalf("empty body = %v, want ErrBodyInvalid", err)
	}

	if _, c, _ := artifactCounts(t, pool, artID); c != 3 {
		t.Fatalf("comment_count = %d, want 3", c)
	}

	// Threads read in order: each root immediately followed by its replies.
	list, err := svc.ListComments(ctx, "SVCAAAA4")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	gotIDs := make([]int64, len(list))
	for i, c := range list {
		gotIDs[i] = c.ID
	}
	wantIDs := []int64{root1.ID, reply1.ID, root2.ID}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("listed %d comments, want %d", len(gotIDs), len(wantIDs))
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("thread order = %v, want %v", gotIDs, wantIDs)
		}
	}
	if list[2].OnBehalfOf != "agent-7" {
		t.Fatalf("root2 on_behalf_of = %q, want agent-7 (provenance must round-trip)", list[2].OnBehalfOf)
	}

	// Edits are author-only and stamp edited_at.
	if err := svc.EditComment(ctx, "SVCAAAA4", root1.ID, "u2", "hijack"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("foreign edit = %v, want ErrForbidden", err)
	}
	if err := svc.EditComment(ctx, "SVCAAAA4", root1.ID, "u1", "root one, edited"); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Soft delete is author-only, tombstones the root, keeps the reply, and
	// decrements the rollup in the same transaction.
	if err := svc.DeleteComment(ctx, "SVCAAAA4", root1.ID, "u2"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("foreign delete = %v, want ErrForbidden", err)
	}
	if err := svc.DeleteComment(ctx, "SVCAAAA4", root1.ID, "u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteComment(ctx, "SVCAAAA4", root1.ID, "u1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}

	list, err = svc.ListComments(ctx, "SVCAAAA4")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("tombstoned thread lists %d comments, want 3", len(list))
	}
	if !list[0].Deleted || list[0].Body != "" {
		t.Fatalf("deleted root = %+v, want tombstone with empty body", list[0])
	}
	if list[1].ID != reply1.ID || list[1].Deleted {
		t.Fatalf("reply under tombstoned root = %+v, want live reply %d", list[1], reply1.ID)
	}
	if list[1].EditedAt != nil {
		t.Fatalf("reply edited_at = %v, want nil", list[1].EditedAt)
	}
	if _, c, _ := artifactCounts(t, pool, artID); c != 2 {
		t.Fatalf("comment_count after soft delete = %d, want 2", c)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestServicePinCount exercises the third rollup: image-region annotations of
// either kind count as pins (ADR-0006: pin_count = image-region reactions +
// pinned comments), tracked through add, un-react, and soft delete.
func TestServicePinCount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	artID := insertArtifact(t, pool, "SVCAAAA5", sharetype.KeyImage)

	region := json.RawMessage(`{"x":0.42,"y":0.31}`)
	if _, _, err := svc.React(ctx, "SVCAAAA5", sharetype.AnchorImageRegion, region, "🔥", "u1"); err != nil {
		t.Fatalf("pin react: %v", err)
	}
	pin, err := svc.AddComment(ctx, "SVCAAAA5", CommentInput{
		AnchorType: sharetype.AnchorImageRegion, AnchorRef: region,
		ActorID: "u2", Body: "nice detail",
	})
	if err != nil {
		t.Fatalf("pin comment: %v", err)
	}
	// A whole-artifact comment is not a pin.
	if _, err := svc.AddComment(ctx, "SVCAAAA5", CommentInput{
		AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "overall gorgeous",
	}); err != nil {
		t.Fatalf("artifact comment: %v", err)
	}

	if r, c, p := artifactCounts(t, pool, artID); r != 1 || c != 2 || p != 2 {
		t.Fatalf("counts = (%d,%d,%d), want (1,2,2)", r, c, p)
	}

	if err := svc.DeleteComment(ctx, "SVCAAAA5", pin.ID, "u2"); err != nil {
		t.Fatalf("delete pin comment: %v", err)
	}
	if removed, err := svc.Unreact(ctx, "SVCAAAA5", sharetype.AnchorImageRegion, region, "🔥", "u1"); err != nil || !removed {
		t.Fatalf("unreact pin: removed=%v err=%v", removed, err)
	}
	if r, c, p := artifactCounts(t, pool, artID); r != 0 || c != 1 || p != 0 {
		t.Fatalf("counts after removals = (%d,%d,%d), want (0,1,0)", r, c, p)
	}
	assertCountsMatchAggregates(t, pool, artID)
}

// TestServiceExpiredArtifactUniformNotFound asserts annotation writes treat an
// expired artifact exactly like an unknown one (ADR-0007: expiry is hard
// non-existence; probing leaks no signal).
func TestServiceExpiredArtifactUniformNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	insertArtifact(t, pool, "SVCAAAA6", sharetype.KeyMarkdown)
	if _, err := pool.Exec(ctx,
		`UPDATE artifacts SET expires_at = now() - interval '1 minute' WHERE public_id = 'SVCAAAA6'`); err != nil {
		t.Fatalf("expire artifact: %v", err)
	}

	if _, _, err := svc.React(ctx, "SVCAAAA6", sharetype.AnchorArtifact, nil, "🔥", "u1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("react on expired artifact = %v, want ErrNotFound", err)
	}
	if _, err := svc.AddComment(ctx, "SVCAAAA6", CommentInput{
		AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "hello?",
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("comment on expired artifact = %v, want ErrNotFound", err)
	}
	if _, err := svc.ListComments(ctx, "SVCAAAA6"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("list on expired artifact = %v, want ErrNotFound", err)
	}
}
