package annotation

// Comment Redaction Tests
//
// A comment body is masked before it is stored (SPEC-0017 RD-1, RD-4): the
// stored row, the returned comment and every listing hold [REDACTED] and never
// the token, the outcome commits with the comment, an edit is scanned like a
// create, and a scan that fails refuses the write. Credentials are assembled
// at run time from split literals.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-9, REQ "Error Handling Standards"
//
// @joestump 09/25/2026 - Added for cairn#290 (unwired paths record unscanned).
// @joestump 09/26/2026 - cairn#291 wires the scanner in; the tests below it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// plantedToken is a GitHub personal access token shape gitleaks detects.
func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

// testScanner is a real scanner with the production defaults.
func testScanner(t *testing.T) *redact.Scanner {
	t.Helper()
	sc, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// redactionCount reads one cairn_redactions_total series.
func redactionCount(t *testing.T, r *metrics.Registry, surface metrics.RedactionSurface, outcome metrics.RedactionOutcome) float64 {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "cairn_redactions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["surface"] == string(surface) && labels["outcome"] == string(outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// commentRowJSON renders every comment row whole, so a test can grep all of
// its columns at once.
func commentRowJSON(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(row_to_json(c)::text, E'\n'), '') FROM comments c`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func commentOutcome(t *testing.T, pool *pgxpool.Pool, id int64) (string, int, map[string]int) {
	t.Helper()
	var (
		status string
		count  int
		rules  map[string]int
	)
	if err := pool.QueryRow(context.Background(), `SELECT redaction_status, redaction_count, redaction_rules FROM comments WHERE id = $1`, id).
		Scan(&status, &count, &rules); err != nil {
		t.Fatal(err)
	}
	return status, count, rules
}

// TestUnwiredCommentRecordsUnscanned: a Service built without a scanner
// records the outcome "unscanned", never "clean".
func TestUnwiredCommentRecordsUnscanned(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default())
	insertArtifact(t, pool, "REDACTC1", sharetype.KeyCode)

	c, err := svc.AddComment(ctx, "REDACTC1", CommentInput{AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "looks fine"})
	if err != nil {
		t.Fatal(err)
	}
	status, count, rules := commentOutcome(t, pool, c.ID)
	if status != "unscanned" || count != 0 || rules == nil || len(rules) != 0 {
		t.Errorf("comment outcome = %s, %d, %v; want unscanned, 0, {}", status, count, rules)
	}
}

// TestCommentStoresOnlyMaskedText: SPEC-0017 RD-1 "Event carries only stored
// text". Comments emit no event yet (SPEC-0016), so the assertion is on
// everything downstream of the write: the stored row, the returned comment and
// the listing all carry [REDACTED], and none of them the token.
func TestCommentStoresOnlyMaskedText(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	reg := metrics.New()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), reg))
	insertArtifact(t, pool, "REDACTC2", sharetype.KeyMarkdown)
	tok := plantedToken(1)

	c, err := svc.AddComment(ctx, "REDACTC2", CommentInput{
		AnchorType: sharetype.AnchorArtifact, ActorID: "u1",
		Body: "rotate this, it leaked: " + tok + " (sorry)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "rotate this, it leaked: [REDACTED] (sorry)"; c.Body != want {
		t.Errorf("returned body = %q, want %q", c.Body, want)
	}
	if c.Redaction.Status != redact.StatusMasked || c.Redaction.Count != 1 || c.Redaction.Rules["github-pat"] != 1 {
		t.Errorf("returned outcome = %+v; want masked, 1, github-pat", c.Redaction)
	}
	status, count, rules := commentOutcome(t, pool, c.ID)
	if status != "masked" || count != 1 || rules["github-pat"] != 1 {
		t.Errorf("stored outcome = %s, %d, %v; want masked, 1, github-pat", status, count, rules)
	}
	listed, err := svc.ListComments(ctx, "REDACTC2")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !strings.Contains(listed[0].Body, redact.Mask) {
		t.Errorf("listed = %+v; want the masked comment", listed)
	}
	for where, text := range map[string]string{"stored row": commentRowJSON(t, pool), "returned body": c.Body, "listed body": listed[0].Body} {
		if strings.Contains(text, tok) || strings.Contains(text, tok[4:24]) {
			t.Errorf("%s holds the token", where)
		}
	}
	if got := redactionCount(t, reg, metrics.SurfaceComment, metrics.OutcomeMasked); got != 1 {
		t.Errorf("cairn_redactions_total{comment,masked} = %v, want 1", got)
	}
}

// TestCleanCommentRecordsClean: a scanned comment with nothing in it records
// "clean", and its body is stored as written.
func TestCleanCommentRecordsClean(t *testing.T) {
	pool := newTestPool(t)
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	insertArtifact(t, pool, "REDACTC3", sharetype.KeyMarkdown)

	c, err := svc.AddComment(context.Background(), "REDACTC3", CommentInput{AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "LGTM"})
	if err != nil {
		t.Fatal(err)
	}
	if status, count, _ := commentOutcome(t, pool, c.ID); status != "clean" || count != 0 || c.Body != "LGTM" {
		t.Errorf("outcome = %s, %d, body %q; want clean, 0, LGTM", status, count, c.Body)
	}
}

// TestEditedCommentIsMasked: an edit is scanned like a create, and its
// outcome replaces the old one with the new body.
func TestEditedCommentIsMasked(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), nil))
	insertArtifact(t, pool, "REDACTC4", sharetype.KeyMarkdown)
	tok := plantedToken(2)

	c, err := svc.AddComment(ctx, "REDACTC4", CommentInput{AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "first draft"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EditComment(ctx, "REDACTC4", c.ID, "u1", "use "+tok); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := pool.QueryRow(ctx, `SELECT body FROM comments WHERE id = $1`, c.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "use [REDACTED]" {
		t.Errorf("edited body = %q; want the token masked", body)
	}
	if status, count, rules := commentOutcome(t, pool, c.ID); status != "masked" || count != 1 || rules["github-pat"] != 1 {
		t.Errorf("edited outcome = %s, %d, %v; want masked, 1, github-pat", status, count, rules)
	}
	if strings.Contains(commentRowJSON(t, pool), tok) {
		t.Error("the comments table holds the token")
	}
}

// TestCommentScanFailureFailsClosed: SPEC-0017 "Detector error fails
// closed". A scan that cannot finish (here, a cancelled request) refuses the
// comment as an internal failure: no row, no count bump, nothing unscanned.
func TestCommentScanFailureFailsClosed(t *testing.T) {
	pool := newTestPool(t)
	reg := metrics.New()
	svc := NewService(pool, sharetype.Default(), WithRedaction(testScanner(t), reg))
	insertArtifact(t, pool, "REDACTC5", sharetype.KeyMarkdown)
	tok := plantedToken(3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.AddComment(ctx, "REDACTC5", CommentInput{AnchorType: sharetype.AnchorArtifact, ActorID: "u1", Body: "key " + tok})
	if !errors.Is(err, redact.ErrScanFailed) {
		t.Fatalf("err = %v; want ErrScanFailed", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Error("the error carries the token")
	}
	var rows, comments int
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM comments), (SELECT comment_count FROM artifacts WHERE public_id = 'REDACTC5')`).
		Scan(&rows, &comments); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || comments != 0 {
		t.Errorf("after a failed scan: %d comment rows, comment_count %d; want 0, 0", rows, comments)
	}
	if got := redactionCount(t, reg, metrics.SurfaceComment, metrics.OutcomeFailed); got != 1 {
		t.Errorf("cairn_redactions_total{comment,failed} = %v, want 1", got)
	}
}
