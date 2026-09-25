package annotation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// schemaSeq disambiguates test schemas created within the same nanosecond.
var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) and
// applies the embedded migrations — including 0002_annotations — inside a
// private, per-test Postgres schema. The isolation matters: `go test ./...`
// runs packages in parallel against the one CI database, so these tests must
// never truncate or populate the tables the store package's integration tests
// are using concurrently.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run annotation integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("annotation_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// insertArtifact seeds a minimal artifact row of the given share type and
// returns its internal id.
func insertArtifact(t *testing.T, pool *pgxpool.Pool, publicID string, shareType artifact.ShareType) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO artifacts (public_id, share_type, actor_id, channel, captured_at, owner_id, expires_at)
		VALUES ($1, $2, 'u1', 'cli', now(), 'u1', now() + interval '1 hour')
		RETURNING id`, publicID, string(shareType)).Scan(&id)
	if err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	return id
}

// insertReaction persists a validated anchor as a reaction row using the
// idempotent upsert the per-kind unique index supports (parameterized per
// SPEC-0006 REQ "Database Operation Standards"). The row carries no kind, as
// a row written before kinds were stored does.
func insertReaction(t *testing.T, pool *pgxpool.Pool, a Anchor, emoji, actorID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind) DO NOTHING`,
		a.ArtifactID, string(a.Type), a.Ref, a.Key, emoji, actorID)
	if err != nil {
		t.Fatalf("insert reaction: %v", err)
	}
}

// TestReactionIdempotentAcrossKeyOrder exercises the ADR-0006 idempotency
// design end to end: two anchor_refs differing only in JSON key order derive
// one canonical anchor_key, so the unique constraint collapses the repeated
// react to a single row.
func TestReactionIdempotentAcrossKeyOrder(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reg := sharetype.Default()
	artID := insertArtifact(t, pool, "AAAAAAA1", sharetype.KeyCode)

	a1, err := Validate(reg, artID, sharetype.KeyCode, sharetype.KindReaction,
		sharetype.AnchorCodeRange, json.RawMessage(`{"start":40,"end":47}`))
	if err != nil {
		t.Fatalf("validate 1: %v", err)
	}
	a2, err := Validate(reg, artID, sharetype.KeyCode, sharetype.KindReaction,
		sharetype.AnchorCodeRange, json.RawMessage(`{ "end": 47, "start": 40 }`))
	if err != nil {
		t.Fatalf("validate 2: %v", err)
	}
	if a1.Key != a2.Key {
		t.Fatalf("canonical keys differ across key order: %q vs %q", a1.Key, a2.Key)
	}

	insertReaction(t, pool, a1, "🔥", "u1")
	insertReaction(t, pool, a2, "🔥", "u1") // same actor+emoji+anchor → no-op

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("duplicate react across key orderings produced %d rows, want 1", n)
	}

	// A different actor or emoji is NOT deduped.
	insertReaction(t, pool, a1, "🔥", "u2")
	insertReaction(t, pool, a1, "🎉", "u1")
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("distinct actor/emoji reactions = %d rows, want 3", n)
	}

	// Un-react deletes exactly that row.
	tag, err := pool.Exec(ctx, `
		DELETE FROM reactions
		WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3 AND emoji = $4 AND actor_id = $5`,
		artID, string(a1.Type), a1.Key, "🔥", "u1")
	if err != nil {
		t.Fatalf("un-react: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("un-react affected %d rows, want 1", tag.RowsAffected())
	}
}

// TestAnchorMatrixPersistence drives every share type × anchor × kind combo
// through Validate and persists each accepted anchor into its table, proving
// the substrate stores exactly the registry-legal matrix and the stored
// anchor_ref round-trips in canonical form.
func TestAnchorMatrixPersistence(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reg := sharetype.Default()

	ids := make(map[artifact.ShareType]int64, len(anchorMatrix))
	i := 0
	for shareType := range anchorMatrix {
		ids[shareType] = insertArtifact(t, pool, string(rune('A'+i))+"AAAAAA2", shareType)
		i++
	}

	var accepted, rejected int
	for shareType, caps := range anchorMatrix {
		artID := ids[shareType]
		for _, anchor := range allAnchors {
			for kind, allowedSet := range map[sharetype.AnnotationKind][]sharetype.Anchor{
				sharetype.KindReaction: caps.reactions,
				sharetype.KindComment:  caps.comments,
			} {
				a, err := Validate(reg, artID, shareType, kind, anchor, json.RawMessage(validRefs[anchor]))
				if !contains(allowedSet, anchor) {
					if err == nil {
						t.Errorf("%s/%s/%s: illegal combo accepted", shareType, kind, anchor)
					}
					rejected++
					continue // rejected writes persist nothing, by construction
				}
				if err != nil {
					t.Errorf("%s/%s/%s: legal combo rejected: %v", shareType, kind, anchor, err)
					continue
				}
				if kind == sharetype.KindReaction {
					insertReaction(t, pool, a, "🔥", "u1")
				} else {
					if _, err := pool.Exec(ctx, `
						INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, actor_id, body)
						VALUES ($1, $2, $3, $4, 'u1', 'looks great')`,
						a.ArtifactID, string(a.Type), a.Ref, a.Key); err != nil {
						t.Errorf("%s/%s: insert comment: %v", shareType, anchor, err)
						continue
					}
				}
				accepted++

				// The stored anchor_ref round-trips through JSONB to the same
				// canonical key.
				table := "reactions"
				if kind == sharetype.KindComment {
					table = "comments"
				}
				var storedRef json.RawMessage
				if err := pool.QueryRow(ctx,
					`SELECT anchor_ref FROM `+table+` WHERE artifact_id = $1 AND anchor_type = $2 AND anchor_key = $3`,
					artID, string(anchor), a.Key).Scan(&storedRef); err != nil {
					t.Errorf("%s/%s/%s: read back: %v", shareType, kind, anchor, err)
					continue
				}
				roundTrip, err := CanonicalKey(storedRef)
				if err != nil || roundTrip != a.Key {
					t.Errorf("%s/%s/%s: stored ref %s round-trips to %q, want %q (err=%v)",
						shareType, kind, anchor, storedRef, roundTrip, a.Key, err)
				}
			}
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("matrix exercised accepted=%d rejected=%d; both must be non-zero", accepted, rejected)
	}

	// Counts in the tables match the accepted writes exactly.
	var reactions, comments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions`).Scan(&reactions); err != nil {
		t.Fatalf("count reactions: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comments`).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if reactions+comments != accepted {
		t.Fatalf("persisted rows = %d, want %d accepted writes", reactions+comments, accepted)
	}
}

// TestCommentThreadSubstrate exercises the comments table's threading shape:
// parent_id self-reference, soft delete preserving replies, and the
// anchor-scoped index columns.
func TestCommentThreadSubstrate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reg := sharetype.Default()
	artID := insertArtifact(t, pool, "AAAAAAA3", sharetype.KeyMarkdown)

	a, err := Validate(reg, artID, sharetype.KeyMarkdown, sharetype.KindComment,
		sharetype.AnchorTextSelection, json.RawMessage(`{"quote":"ship it","start":10,"end":17}`))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	var rootID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, actor_id, body)
		VALUES ($1, $2, $3, $4, 'u1', 'root comment') RETURNING id`,
		a.ArtifactID, string(a.Type), a.Ref, a.Key).Scan(&rootID); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	// A reply shares its root's anchor (SPEC-0006 "Reply to a comment").
	var replyID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, parent_id, actor_id, body)
		VALUES ($1, $2, $3, $4, $5, 'u2', 'agreed') RETURNING id`,
		a.ArtifactID, string(a.Type), a.Ref, a.Key, rootID).Scan(&replyID); err != nil {
		t.Fatalf("insert reply: %v", err)
	}

	// A reply to a nonexistent parent violates the FK.
	if _, err := pool.Exec(ctx, `
		INSERT INTO comments (artifact_id, anchor_type, anchor_ref, anchor_key, parent_id, actor_id, body)
		VALUES ($1, $2, $3, $4, 999999, 'u2', 'orphan')`,
		a.ArtifactID, string(a.Type), a.Ref, a.Key); err == nil {
		t.Fatal("reply referencing a missing parent must be rejected by the FK")
	}

	// Soft-delete the root: the tombstone stays and the reply remains resolvable.
	if _, err := pool.Exec(ctx, `UPDATE comments SET deleted_at = now() WHERE id = $1`, rootID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	var liveReplies int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM comments WHERE parent_id = $1 AND deleted_at IS NULL`, rootID,
	).Scan(&liveReplies); err != nil {
		t.Fatalf("count replies: %v", err)
	}
	if liveReplies != 1 {
		t.Fatalf("soft-deleted root retains %d live replies, want 1", liveReplies)
	}
}

// TestArtifactDeleteCascades asserts deleting an artifact removes its
// annotation rows via ON DELETE CASCADE.
func TestArtifactDeleteCascades(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reg := sharetype.Default()
	artID := insertArtifact(t, pool, "AAAAAAA4", sharetype.KeyWebhook)

	a, err := Validate(reg, artID, sharetype.KeyWebhook, sharetype.KindReaction,
		sharetype.AnchorWebhookRequest, json.RawMessage(`{"request_id":"req_7Kx9"}`))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	insertReaction(t, pool, a, "👀", "u1")

	if _, err := pool.Exec(ctx, `DELETE FROM artifacts WHERE id = $1`, artID); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("reactions must cascade with their artifact, found %d rows", n)
	}
}
