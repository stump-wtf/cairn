package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/cairn/internal/errs"
)

// DeleteArtifact deletes an artifact, restricted to the owning principal, and
// garbage-collects any body blob whose content-hash reference count across all
// live artifacts and bundle members reaches zero. A blob still referenced by
// another live artifact is retained.
//
// Expiry is honored as hard deletion elsewhere by the reaper (SPEC-0009); this
// is the interactive owner-initiated delete. Annotation rows (SPEC-0006) are
// removed by ON DELETE CASCADE once that table exists; provenance lives inline
// on the artifact row and is removed with it.
//
// A non-owner or unknown id returns a uniform not-found so a delete probe leaks
// no more than a read would (ADR-0005/ADR-0007).
//
// Governing: ADR-0008 (reference-counted GC), SPEC-0002 REQ "Artifact Lifecycle
// — Delete and Expiry".
func (s *Store) DeleteArtifact(ctx context.Context, publicID, ownerID string) error {
	if publicID == "" || ownerID == "" {
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
		 WHERE public_id = $1 AND owner_id = $2
		 FOR UPDATE`,
		publicID, ownerID,
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

	// For each referenced blob, GC it iff nothing live references it any more.
	var orphanKeys []string
	for sha := range candidateSHAs {
		var referenced bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM artifacts WHERE body_sha256 = $1)
			     OR EXISTS(SELECT 1 FROM bundle_members WHERE blob_sha256 = $1)`,
			sha,
		).Scan(&referenced); err != nil {
			return fmt.Errorf("delete: refcount %s: %w", sha, err)
		}
		if referenced {
			continue // still referenced by another live artifact/member
		}
		var storageKey string
		if err := tx.QueryRow(ctx,
			`DELETE FROM blobs WHERE sha256 = $1 RETURNING storage_key`, sha,
		).Scan(&storageKey); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // already gone
			}
			return fmt.Errorf("delete: drop blob %s: %w", sha, err)
		}
		orphanKeys = append(orphanKeys, storageKey)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete: commit: %w", err)
	}

	// Remove the now-unreferenced objects after the metadata commit. Postgres is
	// the authority; a failure here only leaves GC-collectable debris, never a
	// dangling reference. Detach from the request context so a nearly-cancelled
	// request still cleans up.
	for _, key := range orphanKeys {
		_ = s.obj.Remove(context.WithoutCancel(ctx), key)
	}
	return nil
}
