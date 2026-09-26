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

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/metrics"
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
	// Tags are client-asserted routing strings (ADR-0018). Unlike Provenance
	// the adapter passes them through from the request as-is; CreateArtifact
	// normalizes them.
	Tags []string
	// RedactionDowngrade is the writer's per-request downgrade of a
	// reject-mode share type to mask (X-Cairn-Redaction: mask, or MCP
	// redaction: "mask"). It never disables the scan. SPEC-0017 RD-5.
	RedactionDowngrade bool
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
// SPEC-0002 REQ "Database Operation Standards",
// ADR-0023, SPEC-0017 RD-1 (the title and body are scanned first; scan.go)
func (s *Store) CreateArtifact(ctx context.Context, in CreateArtifactInput) (*artifact.Artifact, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	// Normalized before the body streams, so a bad tag costs nothing;
	// Artifact.Validate re-checks the result as an aggregate invariant.
	//
	// Governing: ADR-0018, SPEC-0002 REQ "Artifact Tags"
	tags, err := artifact.NormalizeTags(in.Tags)
	if err != nil {
		return nil, err
	}
	// 1. Stream the body to a staging object: compute SHA-256 incrementally
	//    and enforce the size limit as bytes arrive. The object is NOT promoted
	//    to its content-addressed key yet — that happens in CommitBlob under the
	//    blob-row lock so the promotion is serialized against the reaper (§2 of
	//    the SPEC-0009 retention design).
	staged, err := stageBlobKeep(ctx, s.obj, in.Body, s.maxBytes, in.DeclaredMediaType, scanInMemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("create: stream body: %w", err)
	}
	// The staging object is transient on EVERY path (committed OR rolled back);
	// reclaim it unconditionally so no staging/<rand> orphan the reaper cannot
	// reach survives. WithoutCancel is handled inside Discard.
	//
	// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
	defer staged.Discard(ctx, s.obj)

	// 2. Verify a client-declared checksum, if one was provided. Neither digest
	//    goes into the error: it is logged, and both are digests of the raw,
	//    unscanned body, which for a body that is only a credential is a
	//    fingerprint of that credential.
	//
	// Governing: ADR-0023, SPEC-0017 RD-9, RD-10
	if in.ExpectedSHA256 != "" && !strings.EqualFold(in.ExpectedSHA256, staged.SHA256) {
		return nil, fmt.Errorf("create: declared checksum does not match the received body: %w",
			errs.ErrChecksumMismatch)
	}

	// 2b. Scan the title, then the verified bytes, before anything is promoted
	//     or committed. Both use the body's mode, which needs its media type.
	//     A mask repoints staged at the masked copy, so the SHA-256, size and
	//     dedup key below are the stored bytes'; a rejection returns here, and
	//     the deferred Discard removes the staging object.
	//
	// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-5, RD-6, RD-7, RD-10
	var scans scanReport
	mode := s.artifactRedactionMode(in.ShareType, staged.MediaType, in.RedactionDowngrade)
	title, titleScan, err := s.scanTitle(ctx, metrics.SurfaceArtifact, in.Title, mode, &scans)
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	bodyScan, err := s.scanBody(ctx, metrics.SurfaceArtifact, bodyField, staged, mode, &scans)
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}

	// Decide previewability at ingest from the share type + sniffed/declared
	// media type and the preview size bound, via the registry (no switch on
	// type). A non-previewable body becomes the generic file type (FILE/GZ).
	// The decision is stored so every surface agrees without re-sniffing.
	//
	// Governing: ADR-0002 (share-type registry), SPEC-0002 REQ "Previewability
	// Detection at Ingest".
	effectiveType, previewable := s.registry.DecidePreview(
		in.ShareType, staged.MediaType, staged.Size, s.previewMax)

	art := &artifact.Artifact{
		ShareType:   effectiveType,
		Title:       title,
		BodySHA256:  staged.SHA256,
		Size:        staged.Size,
		MediaType:   staged.MediaType,
		Previewable: previewable,
		Provenance:  in.Provenance,
		Access:      in.Access,
		Tags:        tags,
		ExpiresAt:   in.ExpiresAt,
		// Written with the row, in the same transaction (SPEC-0017 RD-9).
		Redaction: bodyScan.Merge(titleScan),
	}

	// 3. Persist metadata atomically: lock/register the blob row and promote its
	//    object under that lock (CommitBlob), then insert the artifact — all in
	//    one transaction so a live reference and its object commit together and
	//    the reaper can never delete the object mid-create.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	if err := CommitBlob(ctx, tx, s.obj, staged); err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}

	bodySHA := &art.BodySHA256
	if err := s.insertArtifact(ctx, tx, art, bodySHA); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create: commit: %w", err)
	}
	s.emitCreated(art)
	s.warnUnscanned(art.PublicID, &scans)
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
			 previewable, actor_id, on_behalf_of, model, channel, captured_at,
			 owner_id, visibility, expires_at, tags,
			 redaction_status, redaction_count, redaction_rules)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id, created_at`

	tags := tagsParam(art.Tags)
	// The outcome commits with the row it describes (SPEC-0017 "Database
	// Operation Standards"). An unset Redaction is written as "unscanned".
	//
	// Governing: ADR-0023, SPEC-0017 RD-9
	rStatus, rCount, rRules, err := redactionColumns(art.Redaction)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

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
			art.Provenance.OnBehalfOf, art.Provenance.Model, art.Provenance.Channel,
			art.Provenance.CapturedAt, art.Access.OwnerID, art.Access.Visibility,
			art.ExpiresAt, tags, rStatus, rCount, rRules,
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

// tagsParam passes tags for the TEXT[] column. pgx encodes a nil slice as SQL
// NULL, which the NOT NULL column rejects, so untagged is always an empty array.
func tagsParam(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// isPublicIDConflict reports whether err is a unique-violation on public_id.
func isPublicIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == "artifacts_public_id_key"
	}
	return false
}
