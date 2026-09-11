package store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// GetByPublicID resolves an artifact by its public id, returning a uniform
// not-found for unknown, unauthorized, and expired ids alike so probing leaks
// no signal. Expiry is treated as hard non-existence at read time.
//
// Governing: ADR-0005 (Short Opaque Identifiers), ADR-0007 (Link Access),
// SPEC-0002 REQ "Artifact Lifecycle — Read"
func (s *Store) GetByPublicID(ctx context.Context, publicID string) (*artifact.Artifact, error) {
	const q = `
		SELECT id, public_id, share_type, title, body_sha256, size_bytes,
		       media_type, previewable, actor_id, on_behalf_of, model, channel,
		       captured_at, owner_id, visibility,
		       reaction_count, comment_count, pin_count, tags, expires_at, created_at
		FROM artifacts
		WHERE public_id = $1 AND expires_at > now()`

	var (
		a       artifact.Artifact
		bodySHA *string
	)
	err := s.pool.QueryRow(ctx, q, publicID).Scan(
		&a.ID, &a.PublicID, &a.ShareType, &a.Title, &bodySHA, &a.Size,
		&a.MediaType, &a.Previewable, &a.Provenance.ActorID,
		&a.Provenance.OnBehalfOf, &a.Provenance.Model, &a.Provenance.Channel, &a.Provenance.CapturedAt,
		&a.Access.OwnerID, &a.Access.Visibility,
		&a.ReactionCount, &a.CommentCount, &a.PinCount, &a.Tags,
		&a.ExpiresAt, &a.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve artifact %s: %w", publicID, errs.ErrNotFound)
		}
		return nil, fmt.Errorf("resolve artifact %s: %w", publicID, err)
	}
	if bodySHA != nil {
		a.BodySHA256 = *bodySHA
	}
	return &a, nil
}

// BodyInfo describes a downloadable body so a reader can re-verify it against
// the stored SHA-256.
type BodyInfo struct {
	SHA256    string
	Size      int64
	MediaType string
}

// OpenBody resolves an artifact and opens its raw body for streaming download.
// The caller must Close the returned reader. Bundles (no single body) return a
// validation error. The returned SHA-256 is the value a reader hashes to verify
// round-trip integrity.
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Content Addressing and Blobs" (round-trip integrity)
func (s *Store) OpenBody(ctx context.Context, publicID string) (io.ReadCloser, BodyInfo, error) {
	a, err := s.GetByPublicID(ctx, publicID)
	if err != nil {
		return nil, BodyInfo{}, err
	}
	if a.BodySHA256 == "" {
		return nil, BodyInfo{}, errs.Validationf("artifact %s has no single body", publicID)
	}

	var storageKey string
	err = s.pool.QueryRow(ctx,
		`SELECT storage_key FROM blobs WHERE sha256 = $1`, a.BodySHA256,
	).Scan(&storageKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, BodyInfo{}, fmt.Errorf("blob %s for artifact %s: %w", a.BodySHA256, publicID, errs.ErrNotFound)
		}
		return nil, BodyInfo{}, fmt.Errorf("load blob %s: %w", a.BodySHA256, err)
	}

	rc, err := s.obj.Get(ctx, storageKey)
	if err != nil {
		return nil, BodyInfo{}, fmt.Errorf("open body %s: %w", publicID, err)
	}
	return rc, BodyInfo{SHA256: a.BodySHA256, Size: a.Size, MediaType: a.MediaType}, nil
}
