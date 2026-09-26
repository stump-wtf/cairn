package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// resolveOwned resolves publicID to its internal id inside tx, locking the row
// (FOR UPDATE) for the mutation that follows, and enforces ownership. An
// unknown or expired id is a uniform errs.ErrNotFound (consistent with a plain
// read — ADR-0007 leaks nothing about existence). An id that DOES exist but is
// owned by someone else is a DISTINCT errs.ErrForbidden, deliberately unlike
// DeleteArtifact's uniform-404-for-non-owner: SPEC-0009 Security Requirements
// "Non-owner policy change" mandates a 403 here (the caller already
// authenticated and is asking about a specific id it believes it owns, so
// hiding existence buys no capability-URL security the way it does for an
// anonymous link-capability read).
//
// Governing: SPEC-0009 REQ "Owner-Only Policy Changes", REQ "Error Handling
// Standards" ("distinct not-owner error"), REQ "Database Operation Standards"
// (row-locked, transactional multi-step mutation).
func (s *Store) resolveOwned(ctx context.Context, tx pgx.Tx, publicID, ownerID string) (internalID int64, err error) {
	var owner string
	err = tx.QueryRow(ctx,
		`SELECT id, owner_id FROM artifacts WHERE public_id = $1 AND expires_at > now() FOR UPDATE`,
		publicID,
	).Scan(&internalID, &owner)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("resolve artifact %s: %w", publicID, errs.ErrNotFound)
		}
		return 0, fmt.Errorf("resolve artifact %s: %w", publicID, err)
	}
	if owner != ownerID {
		return 0, fmt.Errorf("policy change on artifact %s: %w", publicID, errs.ErrForbidden)
	}
	return internalID, nil
}

// UpdateVisibility changes an artifact's link-access visibility (sharing),
// owner-only. Provenance/ownership are never touched — only the visibility
// column moves within the allowed artifact.Visibility set.
//
// Governing: SPEC-0009 REQ "Owner-Only Policy Changes" ("New artifacts MUST
// default to you + anyone with link ... Owner restricts to owner-only"),
// ADR-0007 (access policy).
func (s *Store) UpdateVisibility(ctx context.Context, publicID, ownerID string, vis artifact.Visibility) (*artifact.Artifact, error) {
	if publicID == "" || ownerID == "" {
		return nil, errs.Validationf("policy: id and owner are required")
	}
	if vis != artifact.VisibilityLink && vis != artifact.VisibilityPrivate {
		return nil, errs.Validationf("policy: visibility must be %q or %q", artifact.VisibilityLink, artifact.VisibilityPrivate)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	internalID, err := s.resolveOwned(ctx, tx, publicID, ownerID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE artifacts SET visibility = $1 WHERE id = $2`, vis, internalID); err != nil {
		return nil, fmt.Errorf("policy: update visibility %s: %w", publicID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("policy: commit: %w", err)
	}
	return s.GetByPublicID(ctx, publicID)
}

// UpdateTTL extends or shortens an artifact's expiry, owner-only. expiresAt is
// the new absolute expiry the caller (the adapter) has already computed from
// its own clock plus the requested TTL and validated against the deployment's
// max-TTL cap — the store enforces only the aggregate's own invariants (a
// non-zero expiry strictly in the future; ADR-0007 gives no artifact
// no-expiry/immortal option, so a caller cannot pass a zero time meaning
// "never").
//
// Governing: SPEC-0009 REQ "Default 7-Day TTL, Owner-Adjustable, Visible
// Countdown" ("owner adjusts TTL ... MUST update expires_at and reflect the
// new countdown everywhere"), REQ "Owner-Only Policy Changes".
func (s *Store) UpdateTTL(ctx context.Context, publicID, ownerID string, expiresAt time.Time) (*artifact.Artifact, error) {
	if publicID == "" || ownerID == "" {
		return nil, errs.Validationf("ttl: id and owner are required")
	}
	if expiresAt.IsZero() {
		return nil, errs.Validationf("ttl: expiry is required")
	}
	if !expiresAt.After(time.Now()) {
		return nil, errs.Validationf("ttl: expiry must be in the future")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ttl: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	internalID, err := s.resolveOwned(ctx, tx, publicID, ownerID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE artifacts SET expires_at = $1 WHERE id = $2`, expiresAt, internalID); err != nil {
		return nil, fmt.Errorf("ttl: update expiry %s: %w", publicID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ttl: commit: %w", err)
	}
	return s.GetByPublicID(ctx, publicID)
}

// RotateID mints a fresh public id and atomically replaces the artifact's old
// one, retiring the old id (via retired_ids / freshPublicID) so it cannot be
// re-minted within retiredIDGrace — the revoke-a-leaked-link primitive
// (ADR-0005, SPEC-0009 REQ "Id Rotation as Revoke-a-Leaked-Link"). Owner-only,
// same distinct not-owner semantics as UpdateVisibility/UpdateTTL.
//
// The internal primary key never changes, and every child row (annotations,
// bundle members, trajectory spans, webhook captures) is foreign-keyed to
// THAT internal id, never to public_id — so this single-column UPDATE on
// artifacts is enough to "preserve the artifact and its annotations under the
// new id" with no child-table rewrite whatsoever. The old id resolves nowhere
// after commit (GetByPublicID's `WHERE public_id = $1` simply finds no row),
// which is the same uniform 404 an unknown/expired id gets — old links 404
// uniformly, leaking no signal that the id was ever valid.
func (s *Store) RotateID(ctx context.Context, publicID, ownerID string) (*artifact.Artifact, error) {
	if publicID == "" || ownerID == "" {
		return nil, errs.Validationf("rotate: id and owner are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("rotate: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	internalID, err := s.resolveOwned(ctx, tx, publicID, ownerID)
	if err != nil {
		return nil, err
	}

	var newPublicID string
	for attempt := 0; attempt < idMaxAttempts; attempt++ {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("rotate: savepoint: %w", err)
		}
		pid, err := s.freshPublicID(ctx, sp)
		if err != nil {
			_ = sp.Rollback(ctx)
			return nil, fmt.Errorf("rotate: generate id: %w", err)
		}
		_, err = sp.Exec(ctx, `UPDATE artifacts SET public_id = $1 WHERE id = $2`, pid, internalID)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isPublicIDConflict(err) {
				continue // regenerate and retry
			}
			return nil, fmt.Errorf("rotate: update public id %s: %w", publicID, err)
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, fmt.Errorf("rotate: release savepoint: %w", err)
		}
		newPublicID = pid
		break
	}
	if newPublicID == "" {
		return nil, fmt.Errorf("rotate: exhausted %d id attempts: %w", idMaxAttempts, errs.ErrConflict)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO retired_ids (public_id) VALUES ($1) ON CONFLICT (public_id) DO NOTHING`,
		publicID,
	); err != nil {
		return nil, fmt.Errorf("rotate: retire old id %s: %w", publicID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rotate: commit: %w", err)
	}
	return s.GetByPublicID(ctx, newPublicID)
}
