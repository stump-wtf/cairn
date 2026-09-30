package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// DeleteArtifact deletes an artifact, restricted to the owning principal, and
// garbage-collects the *metadata* row of any body blob whose content-hash
// reference count across all live artifacts and bundle members reaches zero. A
// blob still referenced by another live artifact is retained. The FK from
// artifacts/bundle_members to blobs(sha256) makes this refcount race-safe at the
// metadata layer.
//
// The storage object is deliberately NOT removed here. A byte-identical
// concurrent upload may have dedup'd against the existing object (skipping
// re-upload, ADR-0008) and be about to commit its own reference; deleting the
// object inline would strand that new reference. Orphaned objects are swept by
// the delayed reference-counted reaper and the object-storage lifecycle backstop
// (#32), whose TTL exceeds the maximum artifact TTL so a live body is never
// reaped.
//
// Expiry is honored as hard deletion elsewhere by the reaper (SPEC-0009); this
// is the interactive owner-initiated delete. Annotation rows (SPEC-0006) are
// removed by ON DELETE CASCADE once that table exists; provenance lives inline
// on the artifact row and is removed with it.
//
// A non-owner or unknown id returns a uniform not-found so a delete probe leaks
// no more than a read would (ADR-0005/ADR-0007).
//
// Governing: ADR-0008 (reference-counted GC; delayed reaper tolerates dedup
// races), SPEC-0002 REQ "Artifact Lifecycle — Delete and Expiry", SPEC-0009 REQ
// "Object-Storage Lifecycle Backstop" (#32).
func (s *Store) DeleteArtifact(ctx context.Context, publicID, ownerUserID string) error {
	if publicID == "" || ownerUserID == "" {
		return errs.Validationf("delete: id and owner are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id      int64
		bodySHA *string
	)
	err = tx.QueryRow(ctx,
		`SELECT id, body_sha256 FROM artifacts
		 WHERE public_id = $1 AND owner_user_id = $2
		 FOR UPDATE`,
		publicID, user.IDParam(ownerUserID),
	).Scan(&id, &bodySHA)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delete artifact %s: %w", publicID, errs.ErrNotFound)
		}
		return fmt.Errorf("delete: load artifact %s: %w", publicID, err)
	}

	// Gather every blob this artifact references (its body plus any bundle
	// members) so we can re-check their reference counts after deletion.
	candidateSHAs := map[string]struct{}{}
	if bodySHA != nil && *bodySHA != "" {
		candidateSHAs[*bodySHA] = struct{}{}
	}
	memberRows, err := tx.Query(ctx,
		`SELECT DISTINCT blob_sha256 FROM bundle_members WHERE bundle_id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete: load members: %w", err)
	}
	for memberRows.Next() {
		var sha string
		if err := memberRows.Scan(&sha); err != nil {
			memberRows.Close()
			return fmt.Errorf("delete: scan member: %w", err)
		}
		candidateSHAs[sha] = struct{}{}
	}
	memberRows.Close()
	if err := memberRows.Err(); err != nil {
		return fmt.Errorf("delete: iterate members: %w", err)
	}

	// Delete the artifact; bundle_members cascade (ON DELETE CASCADE).
	if _, err := tx.Exec(ctx, `DELETE FROM artifacts WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete: remove artifact %s: %w", publicID, err)
	}

	// For each referenced blob, drop its metadata row iff nothing live references
	// it any more. Only the row is removed; the storage object is left for the
	// delayed reaper / lifecycle backstop (#32) so a concurrent dedup'd upload that
	// skipped re-upload is never stranded (see the method doc).
	for sha := range candidateSHAs {
		referenced, err := blobStillReferenced(ctx, tx, sha)
		if err != nil {
			return fmt.Errorf("delete: refcount %s: %w", sha, err)
		}
		if referenced {
			continue // still referenced by another live artifact/member
		}
		if _, err := tx.Exec(ctx, `DELETE FROM blobs WHERE sha256 = $1`, sha); err != nil {
			return fmt.Errorf("delete: drop blob %s: %w", sha, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete: commit: %w", err)
	}
	return nil
}
