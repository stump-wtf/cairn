package store

// Redaction Outcome Store Tests
//
// Integration tests for the recorded scan outcome (migration 0017): legacy rows
// read "unscanned" after the migration applies, unwired create paths keep
// writing "unscanned", the write helpers commit with the caller's transaction,
// and no outcome column can hold a planted token. Credentials are assembled at
// run time from split literals, so no source line holds one whole.
//
// Governing: ADR-0023, SPEC-0017 RD-9, "Database Operation Standards"
//
// @joestump 09/25/2026 - Added for cairn#290.

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/redact"
)

// plantedToken returns a GitHub personal access token shape (ghp_ plus 36
// alphanumerics) that gitleaks' github-pat rule detects, assembled at run time.
func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

// outcomeOf reads an artifact's outcome columns straight from the table.
func outcomeOf(t *testing.T, pool *pgxpool.Pool, artID int64) redact.Summary {
	t.Helper()
	var s redact.Summary
	if err := pool.QueryRow(context.Background(),
		`SELECT redaction_status, redaction_count, redaction_rules FROM artifacts WHERE id = $1`, artID,
	).Scan(&s.Status, &s.Count, &s.Rules); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	return s
}

// TestLegacyArtifactReadsUnscanned: an artifact written before the migration
// reads "unscanned" once it has applied (SPEC-0017 "Legacy artifact status").
// The test rolls 0017 back by hand, writes a row the way the old insert did,
// and re-runs the migrator, so it also proves the migration applies to a
// database that already holds rows.
func TestLegacyArtifactReadsUnscanned(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	for _, table := range []string{"artifacts", "bundle_members", "comments", "runs"} {
		if _, err := pool.Exec(ctx, `ALTER TABLE `+table+` DROP COLUMN redaction_status, DROP COLUMN redaction_count, DROP COLUMN redaction_rules`); err != nil {
			t.Fatalf("roll back 0017 on %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version LIKE '%_redaction_outcome'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (public_id, share_type, title, body_sha256, size_bytes, media_type,
		                       previewable, actor_id, channel, captured_at, owner_id, visibility, expires_at)
		VALUES ('legacy01', 'bundle', 'old', NULL, 0, 'application/vnd.cairn.bundle',
		        false, 'u1', 'via CLI', $1, 'u1', 'link', $2)`, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-apply 0017 over existing rows: %v", err)
	}
	a, err := s.GetByPublicID(ctx, "legacy01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Redaction.Status != redact.StatusUnscanned || a.Redaction.Count != 0 || len(a.Redaction.Rules) != 0 {
		t.Errorf("legacy artifact outcome = %+v, want unscanned, 0, no rules", a.Redaction)
	}
}

// TestUnwiredCreateRecordsUnscanned: until a create path runs the scanner, it
// records "unscanned" and never claims "clean".
func TestUnwiredCreateRecordsUnscanned(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	a, err := s.CreateArtifact(ctx, input([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, pool, a.ID); got.Status != redact.StatusUnscanned {
		t.Errorf("single-body create recorded %s, want unscanned", got.Status)
	}
	if a.Redaction.Status != "" && a.Redaction.Status != redact.StatusUnscanned {
		t.Errorf("returned artifact claims %s", a.Redaction.Status)
	}

	b, err := s.CreateBundle(ctx, CreateBundleInput{
		Members:    []MemberInput{{Name: "a.txt", Body: bytes.NewReader([]byte("a"))}},
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, pool, b.ID); got.Status != redact.StatusUnscanned {
		t.Errorf("bundle create recorded %s, want unscanned", got.Status)
	}
	var memberStatus string
	if err := pool.QueryRow(ctx, `SELECT redaction_status FROM bundle_members WHERE bundle_id = $1`, b.ID).Scan(&memberStatus); err != nil {
		t.Fatal(err)
	}
	if memberStatus != string(redact.StatusUnscanned) {
		t.Errorf("bundle member recorded %s, want unscanned", memberStatus)
	}
}

// TestInsertCarriesOutcome: a Redaction set on the artifact (and on a bundle
// member) is written by the insert itself, in the create transaction.
func TestInsertCarriesOutcome(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	want := redact.Summary{Status: redact.StatusMasked, Count: 2, Rules: map[string]int{"github-pat": 2}}

	art := &artifact.Artifact{
		ShareType: artifact.TypeBundle, Title: "t", MediaType: "application/vnd.cairn.bundle",
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
		Redaction:  want,
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.insertArtifact(ctx, tx, art, nil); err != nil {
		t.Fatal(err)
	}
	sb, err := StageBlob(ctx, s.obj, bytes.NewReader([]byte("member")), s.maxBytes, "")
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Discard(ctx, s.obj)
	if err := CommitBlob(ctx, tx, s.obj, sb); err != nil {
		t.Fatal(err)
	}
	if err := insertBundleMember(ctx, tx, art.ID, stagedMember{ordinal: 0, name: "m", blob: sb, redaction: want}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if got := outcomeOf(t, pool, art.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("artifact outcome = %+v, want %+v", got, want)
	}
	var member redact.Summary
	if err := pool.QueryRow(ctx,
		`SELECT redaction_status, redaction_count, redaction_rules FROM bundle_members WHERE bundle_id = $1`, art.ID,
	).Scan(&member.Status, &member.Count, &member.Rules); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(member, want) {
		t.Errorf("member outcome = %+v, want %+v", member, want)
	}
}

// TestRecordRedactionOwnerSummary: RD-9 "Owner sees a summary" at the store
// layer. A body with two planted tokens is masked by the real scanner, its
// outcome is recorded with the helper in the content's transaction, and the
// read reports masked, 2, and the rule twice. No column of any outcome table
// holds any part of either token.
func TestRecordRedactionOwnerSummary(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	scanner, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := plantedToken(1), plantedToken(2)
	masked, o, err := scanner.Text(ctx, "body", "first "+a+"\nsecond "+b+"\n", redact.ModeMask)
	if err != nil {
		t.Fatal(err)
	}

	in := input([]byte(masked))
	in.ShareType = "markdown"
	art, err := s.CreateArtifact(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordRedaction(ctx, tx, RedactionArtifact, art.ID, o.Summary()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	want := redact.Summary{Status: redact.StatusMasked, Count: 2, Rules: map[string]int{"github-pat": 2}}
	if !reflect.DeepEqual(got.Redaction, want) {
		t.Errorf("read outcome = %+v, want %+v", got.Redaction, want)
	}

	// Grep every row of every outcome table, whole, as JSON text.
	for _, table := range []string{"artifacts", "bundle_members", "comments", "runs"} {
		var dump string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(row_to_json(t)::text, E'\n'), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		for _, tok := range []string{a, b} {
			if strings.Contains(dump, tok) || strings.Contains(dump, tok[4:24]) {
				t.Errorf("%s holds part of a planted token", table)
			}
		}
	}
}

// TestRecordRedactionRollsBackWithContent: the outcome belongs to the
// transaction it was written in, so a rolled-back write leaves no outcome.
func TestRecordRedactionRollsBackWithContent(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	art, err := s.CreateArtifact(ctx, input([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordRedaction(ctx, tx, RedactionArtifact, art.ID, redact.Summary{Status: redact.StatusClean}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, pool, art.ID); got.Status != redact.StatusUnscanned {
		t.Errorf("rolled-back outcome persisted: %s", got.Status)
	}
}

// TestAccumulateRedaction: a run's appended spans fold their outcomes into the
// run's recorded one.
func TestAccumulateRedaction(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	art, err := s.CreateArtifact(ctx, input([]byte("run envelope")))
	if err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (artifact_id, started_at, redaction_status) VALUES ($1, now(), 'clean') RETURNING id`, art.ID,
	).Scan(&runID); err != nil {
		t.Fatal(err)
	}

	step := func(sum redact.Summary) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := AccumulateRedaction(ctx, tx, RedactionRun, runID, sum); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step(redact.Summary{Status: redact.StatusMasked, Count: 1, Rules: map[string]int{"github-pat": 1}})
	step(redact.Summary{Status: redact.StatusClean})
	step(redact.Summary{Status: redact.StatusMasked, Count: 2, Rules: map[string]int{"github-pat": 1, "cairn-authorization": 1}})

	var got redact.Summary
	if err := pool.QueryRow(ctx, `SELECT redaction_status, redaction_count, redaction_rules FROM runs WHERE id = $1`, runID).
		Scan(&got.Status, &got.Count, &got.Rules); err != nil {
		t.Fatal(err)
	}
	want := redact.Summary{Status: redact.StatusMasked, Count: 3, Rules: map[string]int{"github-pat": 2, "cairn-authorization": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("run outcome = %+v, want %+v", got, want)
	}
}

// TestRecordRedactionRefusals: an unknown target, a missing row and a value
// that is not a rule ID are all refused, and nothing is written.
func TestRecordRedactionRefusals(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()
	art, err := s.CreateArtifact(ctx, input([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	tok := plantedToken(3)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := RecordRedaction(ctx, tx, "bundle_member", art.ID, redact.Summary{}); err == nil {
		t.Error("unknown target accepted")
	}
	if err := RecordRedaction(ctx, tx, RedactionComment, 999999, redact.Summary{Status: redact.StatusClean}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing row: %v, want not found", err)
	}
	if err := AccumulateRedaction(ctx, tx, RedactionRun, 999999, redact.Summary{Status: redact.StatusClean}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing run: %v, want not found", err)
	}
	err = RecordRedaction(ctx, tx, RedactionArtifact, art.ID,
		redact.Summary{Status: redact.StatusMasked, Count: 1, Rules: map[string]int{tok: 1}})
	if err == nil {
		t.Fatal("a token recorded as a rule ID was accepted")
	}
	if strings.Contains(err.Error(), tok[4:24]) {
		t.Error("the refusal repeats the token")
	}
}
