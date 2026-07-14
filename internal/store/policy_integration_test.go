package store

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// TestUpdateVisibilityOwnerOnly exercises the SPEC-0009 REQ "Owner-Only
// Policy Changes" scenarios: the owner may flip visibility within the
// allowed set, a non-owner is refused with a DISTINCT forbidden (not the
// uniform not-found DeleteArtifact uses), and an unknown/expired id is a
// uniform not-found.
func TestUpdateVisibilityOwnerOnly(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, input([]byte("visibility target")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if art.Access.Visibility != artifact.VisibilityLink {
		t.Fatalf("default visibility = %q, want link", art.Access.Visibility)
	}

	// Non-owner: distinct forbidden, policy unchanged.
	if _, err := s.UpdateVisibility(ctx, art.PublicID, "mallory", artifact.VisibilityPrivate); errs.CodeOf(err) != errs.CodeForbidden {
		t.Fatalf("non-owner update code = %q, want forbidden", errs.CodeOf(err))
	}
	got, err := s.GetByPublicID(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("get after refused update: %v", err)
	}
	if got.Access.Visibility != artifact.VisibilityLink {
		t.Fatalf("visibility changed by a refused non-owner update: %q", got.Access.Visibility)
	}

	// Owner: restricts to owner-only.
	updated, err := s.UpdateVisibility(ctx, art.PublicID, "u1", artifact.VisibilityPrivate)
	if err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if updated.Access.Visibility != artifact.VisibilityPrivate {
		t.Fatalf("visibility = %q, want private", updated.Access.Visibility)
	}
	// Ownership itself must never move.
	if updated.Access.OwnerID != "u1" {
		t.Fatalf("owner changed by a policy update: %q", updated.Access.OwnerID)
	}

	// Owner: back to link.
	updated, err = s.UpdateVisibility(ctx, art.PublicID, "u1", artifact.VisibilityLink)
	if err != nil {
		t.Fatalf("owner update back to link: %v", err)
	}
	if updated.Access.Visibility != artifact.VisibilityLink {
		t.Fatalf("visibility = %q, want link", updated.Access.Visibility)
	}

	// Invalid visibility value: validation_failed, no mutation.
	if _, err := s.UpdateVisibility(ctx, art.PublicID, "u1", "bogus"); errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("invalid visibility code = %q, want validation_failed", errs.CodeOf(err))
	}

	// Unknown id: uniform not-found, same as a read.
	if _, err := s.UpdateVisibility(ctx, "nonexist", "u1", artifact.VisibilityPrivate); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("unknown id code = %q, want not_found", errs.CodeOf(err))
	}
}

// TestUpdateTTLExtendAndShorten exercises the owner extending and shortening
// the TTL (SPEC-0009 REQ "Default 7-Day TTL, Owner-Adjustable, Visible
// Countdown"), and the same owner-only / not-found discipline as visibility.
func TestUpdateTTLExtendAndShorten(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, input([]byte("ttl target")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	original := art.ExpiresAt

	// Non-owner: distinct forbidden, TTL unchanged.
	if _, err := s.UpdateTTL(ctx, art.PublicID, "mallory", time.Now().Add(48*time.Hour)); errs.CodeOf(err) != errs.CodeForbidden {
		t.Fatalf("non-owner update code = %q, want forbidden", errs.CodeOf(err))
	}

	// Owner extends.
	extended := time.Now().Add(30 * 24 * time.Hour)
	got, err := s.UpdateTTL(ctx, art.PublicID, "u1", extended)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if !got.ExpiresAt.After(original) {
		t.Fatalf("extended expiry %s not after original %s", got.ExpiresAt, original)
	}
	if got.ExpiresAt.Sub(extended).Abs() > time.Second {
		t.Fatalf("extended expiry = %s, want ~%s", got.ExpiresAt, extended)
	}

	// Owner shortens.
	shortened := time.Now().Add(10 * time.Minute)
	got, err = s.UpdateTTL(ctx, art.PublicID, "u1", shortened)
	if err != nil {
		t.Fatalf("shorten: %v", err)
	}
	if got.ExpiresAt.Sub(shortened).Abs() > time.Second {
		t.Fatalf("shortened expiry = %s, want ~%s", got.ExpiresAt, shortened)
	}

	// Zero / past expiry rejected.
	if _, err := s.UpdateTTL(ctx, art.PublicID, "u1", time.Time{}); errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("zero expiry code = %q, want validation_failed", errs.CodeOf(err))
	}
	if _, err := s.UpdateTTL(ctx, art.PublicID, "u1", time.Now().Add(-time.Hour)); errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("past expiry code = %q, want validation_failed", errs.CodeOf(err))
	}
}

// TestUpdateTTLShortenToExpiredThenUniformNotFound: shortening TTL to
// something already past the moment of the next read makes the artifact
// resolve as the same uniform not-found an unknown id gets (expiry is hard
// non-existence at read time — read.go).
func TestUpdateTTLShortenToExpiredThenUniformNotFound(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, input([]byte("shorten to expired")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A whisker in the future so UpdateTTL's own future-check accepts it, but
	// it will have elapsed by the time we read it back.
	if _, err := s.UpdateTTL(ctx, art.PublicID, "u1", time.Now().Add(50*time.Millisecond)); err != nil {
		t.Fatalf("shorten near-immediately: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if _, err := s.GetByPublicID(ctx, art.PublicID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("post-shorten-expiry read code = %q, want not_found", errs.CodeOf(err))
	}
}

// TestRotateIDInvalidatesOldPreservesAnnotations is the id-rotation
// integration scenario the issue explicitly calls out: new id resolves, old
// id 404s uniformly, and the artifact's annotations (keyed on the immutable
// internal id, never public_id) follow it under the new id (SPEC-0009 REQ
// "Id Rotation as Revoke-a-Leaked-Link").
func TestRotateIDInvalidatesOldPreservesAnnotations(t *testing.T) {
	s, pool := newTestStore(t, Options{})
	ctx := context.Background()

	art, err := s.CreateArtifact(ctx, input([]byte("rotate target")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	oldID := art.PublicID

	// A reaction row stands in for "its annotations" — reactions/comments both
	// FK on the artifact's internal bigint id, never public_id, so any
	// annotation table demonstrates the same invariant.
	if _, err := pool.Exec(ctx,
		`INSERT INTO reactions (artifact_id, anchor_type, anchor_ref, anchor_key, emoji, actor_id)
		 VALUES ($1, 'artifact', '{}', '', '🔥', 'u1')`, art.ID,
	); err != nil {
		t.Fatalf("seed reaction: %v", err)
	}

	// Non-owner: distinct forbidden, id unchanged.
	if _, err := s.RotateID(ctx, oldID, "mallory"); errs.CodeOf(err) != errs.CodeForbidden {
		t.Fatalf("non-owner rotate code = %q, want forbidden", errs.CodeOf(err))
	}

	rotated, err := s.RotateID(ctx, oldID, "u1")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.PublicID == oldID {
		t.Fatal("rotate must mint a different public id")
	}
	if rotated.ID != art.ID {
		t.Fatalf("rotate must preserve the internal id: got %d, want %d", rotated.ID, art.ID)
	}
	if len(rotated.PublicID) != 8 {
		t.Fatalf("rotated public id %q length = %d, want 8", rotated.PublicID, len(rotated.PublicID))
	}

	// New id resolves.
	got, err := s.GetByPublicID(ctx, rotated.PublicID)
	if err != nil {
		t.Fatalf("resolve new id: %v", err)
	}
	if got.ID != art.ID {
		t.Fatalf("resolved internal id = %d, want %d", got.ID, art.ID)
	}

	// Old id 404s uniformly — same as a never-existed id.
	if _, err := s.GetByPublicID(ctx, oldID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("old id after rotate code = %q, want not_found", errs.CodeOf(err))
	}

	// The annotation followed the internal id, reachable now only under the
	// new public id from a caller's perspective, and unchanged in the DB.
	var reactionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reactions WHERE artifact_id = $1`, art.ID).Scan(&reactionCount); err != nil {
		t.Fatalf("count reactions: %v", err)
	}
	if reactionCount != 1 {
		t.Fatalf("reaction count after rotate = %d, want 1 (annotations must survive rotation)", reactionCount)
	}

	// The retired id is recorded so the generator excludes it from re-minting
	// within the grace window (see TestFreshPublicIDExcludesRetiredID for the
	// exclusion behavior itself).
	var retired bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_ids WHERE public_id = $1)`, oldID).Scan(&retired); err != nil {
		t.Fatalf("check retired_ids: %v", err)
	}
	if !retired {
		t.Fatal("old id must be recorded in retired_ids after rotation")
	}
}

// TestFreshPublicIDExcludesRetiredID drives Store.freshPublicID directly
// against a forced generator sequence that offers an already-retired id
// twice before a fresh one, asserting the retired candidates are skipped
// (ADR-0005 "a retired id is not reused within TTL-plus-grace", SPEC-0009
// REQ "Id Rotation as Revoke-a-Leaked-Link" scenario "Retired id not
// reused").
func TestFreshPublicIDExcludesRetiredID(t *testing.T) {
	ids := []string{"RETIRED1", "RETIRED1", "FRESH999"}
	i := 0
	next := func() (string, error) {
		v := ids[i]
		i++
		return v, nil
	}
	s, pool := newTestStore(t, Options{NewID: next})
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO retired_ids (public_id) VALUES ($1)`, "RETIRED1"); err != nil {
		t.Fatalf("seed retired id: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	got, err := s.freshPublicID(ctx, tx)
	if err != nil {
		t.Fatalf("freshPublicID: %v", err)
	}
	if got != "FRESH999" {
		t.Fatalf("freshPublicID = %q, want FRESH999 (RETIRED1 must be skipped both times it was offered)", got)
	}
}

// TestRotateIDUnknownIsUniformNotFound mirrors GetByPublicID's leak-free
// resolution for the rotate surface: an unknown/expired id refuses with the
// same not-found a read gets, never a distinct signal.
func TestRotateIDUnknownIsUniformNotFound(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	ctx := context.Background()

	if _, err := s.RotateID(ctx, "nonexist", "u1"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("unknown id rotate code = %q, want not_found", errs.CodeOf(err))
	}
}
