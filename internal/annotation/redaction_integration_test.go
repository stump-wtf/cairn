package annotation

import (
	"context"
	"testing"

	"github.com/stump-wtf/cairn/internal/sharetype"
)

// TestUnwiredCommentRecordsUnscanned: a comment's scan outcome is written with
// the comment, and until #291 wires the scanner in it is "unscanned", never
// "clean".
//
// Governing: ADR-0023, SPEC-0017 RD-9
func TestUnwiredCommentRecordsUnscanned(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	insertArtifact(t, pool, "REDACTC1", sharetype.KeyCode)

	c, err := svc.AddComment(ctx, "REDACTC1", CommentInput{AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "looks fine"})
	if err != nil {
		t.Fatal(err)
	}
	var (
		status string
		count  int
		rules  map[string]int
	)
	if err := pool.QueryRow(ctx, `SELECT redaction_status, redaction_count, redaction_rules FROM comments WHERE id = $1`, c.ID).
		Scan(&status, &count, &rules); err != nil {
		t.Fatal(err)
	}
	if status != "unscanned" || count != 0 || rules == nil || len(rules) != 0 {
		t.Errorf("comment outcome = %s, %d, %v; want unscanned, 0, {}", status, count, rules)
	}
}
