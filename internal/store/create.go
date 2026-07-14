package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// CreateArtifactInput is the transport-agnostic create request. Provenance,
// access policy, and expiry are mandatory (SPEC-0002 invariants); the caller
// (an adapter) is responsible for deriving Channel server-side, never from a
// client claim (SPEC-0002 "Channel is server-derived").
type CreateArtifactInput struct {
	ShareType         artifact.ShareType
	Title             string
	Body              io.Reader
	DeclaredMediaType string
	// ExpectedSHA256, if non-empty, is verified against the computed hash on
	// finalize; a mismatch rejects the upload with no persisted state.
	ExpectedSHA256 string
	Provenance     artifact.Provenance
	Access         artifact.AccessPolicy
	ExpiresAt      time.Time
}

func (in CreateArtifactInput) validate() error {
	switch {
	case in.ShareType == "":
		return errs.Validationf("create: missing share type")
	case in.ShareType == artifact.TypeBundle:
		// A bundle is a members-only type built via CreateBundle (NULL body +
		// bundle_members). Accepting it on the single-body path would mint a
		// malformed bundle (a body blob, no members), so reject a client-supplied
		// bundle type here. SPEC-0002 REQ "Bundles with N Members".
		return errs.Validationf("create: bundle artifacts must be created via the bundle (multipart) path")
	case in.Provenance.Channel == "":
		return errs.Validationf("create: provenance channel is required")
	case in.Provenance.ActorID == "":
		return errs.Validationf("create: provenance actor is required")
	case in.Access.OwnerID == "":
		return errs.Validationf("create: access owner is required")
	case in.Access.Visibility == "":
		return errs.Validationf("create: access visibility is required")
	case in.ExpiresAt.IsZero():
		return errs.Validationf("create: expiry is required")
	case in.Body == nil:
		return errs.Validationf("create: nil body")
	}
	return nil
}

// CreateArtifact streams the body to storage with checksum verification and
// persists the artifact and its blob registry row in a single transaction. It
// returns the artifact with its minted public id, immediately resolvable.
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Artifact Lifecycle — Create",
// SPEC-0002 REQ "Streaming Upload with Checksum Verification",
// SPEC-0002 REQ "Database Operation Standards"
func (s *Store) CreateArtifact(ctx context.Context, in CreateArtifactInput) (*artifact.Artifact, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	// 1. Stream the body to a staging object: compute SHA-256 incrementally
	//    and enforce the size limit as bytes arrive.
	staged, err := streamBlob(ctx, s.obj, in.Body, s.maxBytes, in.DeclaredMediaType)
	if err != nil {
		return nil, fmt.Errorf("create: stream body: %w", err)
	}
	// The staging object is transient on EVERY path: the new-blob path promotes
	// it by Copy(staging→final) and the dedup-hit path never promotes it at all,
	// so once it has served its purpose it is pure debris regardless of whether
	// the metadata commit succeeds. Remove it unconditionally on the way out.
	// A committed-gated cleanup leaked one orphan staging/<rand> object per
	// successful upload (staging keys aren't in `blobs`, so the SPEC-0009 reaper
	// can never reclaim them). WithoutCancel so cleanup still runs when the
	// request context is cancelled after commit.
	//
	// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
	defer func() {
		_ = s.obj.Remove(context.WithoutCancel(ctx), staged.stagingKey)
	}()

	// 2. Verify a client-declared checksum, if one was provided.
	if in.ExpectedSHA256 != "" && !strings.EqualFold(in.ExpectedSHA256, staged.sha256) {
		return nil, fmt.Errorf("create: expected %s got %s: %w",
			in.ExpectedSHA256, staged.sha256, errs.ErrChecksumMismatch)
	}

	// 3. Promote the staging object to its content-addressed key unless the
	//    blob object already exists (object-level dedup — never re-upload).
	finalKey := shardedKey(staged.sha256)
	exists, err := s.obj.Stat(ctx, finalKey)
	if err != nil {
		return nil, fmt.Errorf("create: stat blob object %s: %w", staged.sha256, err)
	}
	if !exists {
		if err := s.obj.Copy(ctx, staged.stagingKey, finalKey); err != nil {
			return nil, fmt.Errorf("create: promote blob object %s: %w", staged.sha256, err)
		}
	}

	// Decide previewability at ingest from the share type + sniffed/declared
	// media type and the preview size bound, via the registry (no switch on
	// type). A non-previewable body becomes the generic file type (FILE/GZ).
	// The decision is stored so every surface agrees without re-sniffing.
	//
	// Governing: ADR-0002 (share-type registry), SPEC-0002 REQ "Previewability
	// Detection at Ingest".
	effectiveType, previewable := s.registry.DecidePreview(
		in.ShareType, staged.mediaType, staged.size, s.previewMax)

	art := &artifact.Artifact{
		ShareType:   effectiveType,
		Title:       in.Title,
		BodySHA256:  staged.sha256,
		Size:        staged.size,
		MediaType:   staged.mediaType,
		Previewable: previewable,
		Provenance:  in.Provenance,
		Access:      in.Access,
		ExpiresAt:   in.ExpiresAt,
	}

	// 4. Persist metadata atomically: upsert the blob registry row (dedup via
	//    ON CONFLICT DO NOTHING) and insert the artifact.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	if _, err := tx.Exec(ctx,
		`INSERT INTO blobs (sha256, size_bytes, media_type, storage_key)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (sha256) DO NOTHING`,
		staged.sha256, staged.size, staged.mediaType, finalKey,
	); err != nil {
		return nil, fmt.Errorf("create: upsert blob %s: %w", staged.sha256, err)
	}

	bodySHA := &art.BodySHA256
	if err := s.insertArtifact(ctx, tx, art, bodySHA); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create: commit: %w", err)
	}
	return art, nil
}

// freshPublicID draws a candidate public id from the generator and rejects it
// if it is still a retired id within retiredIDGrace (ADR-0005 "a retired id
// is not reused within TTL-plus-grace"), retrying until it finds one that is
// not. It does not itself guard against colliding with a currently-LIVE id —
// that race is caught by the unique constraint on artifacts.public_id and
// handled by the caller's savepoint-retry loop (isPublicIDConflict); this only
// adds the retired-id exclusion on top, using the caller's tx/savepoint so the
// check is consistent with the write it guards.
func (s *Store) freshPublicID(ctx context.Context, tx pgx.Tx) (string, error) {
	for {
		pid, err := s.newID()
		if err != nil {
			return "", err
		}
		var retired bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM retired_ids WHERE public_id = $1 AND retired_at > now() - ($2 * interval '1 second'))`,
			pid, retiredIDGrace.Seconds(),
		).Scan(&retired); err != nil {
			return "", fmt.Errorf("check retired id: %w", err)
		}
		if retired {
			continue
		}
		return pid, nil
	}
}

// insertArtifact mints a public id and inserts the artifact, regenerating the id
// and retrying via a savepoint on the rare public_id unique conflict. bodySHA is
// the body blob reference, or nil for a bundle (whose body_sha256 column is
// NULL and whose members are inserted separately in the same transaction).
func (s *Store) insertArtifact(ctx context.Context, tx pgx.Tx, art *artifact.Artifact, bodySHA *string) error {
	const insertSQL = `
		INSERT INTO artifacts
			(public_id, share_type, title, body_sha256, size_bytes, media_type,
			 previewable, actor_id, on_behalf_of, channel, captured_at,
			 owner_id, visibility, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id, created_at`

	for attempt := 0; attempt < idMaxAttempts; attempt++ {
		// Savepoint so a unique conflict aborts only this attempt, not the tx.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("create: savepoint: %w", err)
		}
		pid, err := s.freshPublicID(ctx, sp)
		if err != nil {
			_ = sp.Rollback(ctx)
			return fmt.Errorf("create: generate id: %w", err)
		}
		art.PublicID = pid
		if err := art.Validate(); err != nil {
			_ = sp.Rollback(ctx)
			return err // invariant violation, identical every attempt
		}

		err = sp.QueryRow(ctx, insertSQL,
			art.PublicID, art.ShareType, art.Title, bodySHA, art.Size,
			art.MediaType, art.Previewable, art.Provenance.ActorID,
			art.Provenance.OnBehalfOf, art.Provenance.Channel,
			art.Provenance.CapturedAt, art.Access.OwnerID, art.Access.Visibility,
			art.ExpiresAt,
		).Scan(&art.ID, &art.CreatedAt)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isPublicIDConflict(err) {
				continue // regenerate and retry
			}
			return fmt.Errorf("create: insert artifact: %w", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("create: release savepoint: %w", err)
		}
		return nil
	}
	return fmt.Errorf("create: exhausted %d id attempts: %w", idMaxAttempts, errs.ErrConflict)
}

// isPublicIDConflict reports whether err is a unique-violation on public_id.
func isPublicIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == "artifacts_public_id_key"
	}
	return false
}
