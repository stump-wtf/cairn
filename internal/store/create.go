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
	"github.com/stump-wtf/cairn/internal/sharetype"
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
}

// ChecksumHeader is the REST create header that carries ExpectedSHA256. No
// other surface sends a checksum, so a mismatch is reported on it.
const ChecksumHeader = "X-Cairn-Sha256"

// ChecksumMismatch is the violation for a body whose SHA-256 is not the one the
// caller declared. It echoes the digest the caller sent (never the body), and
// errors.Is(err, errs.ErrChecksumMismatch) still holds.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-2
func ChecksumMismatch(expected string) *errs.Invalid {
	return errs.Violate(ChecksumHeader, errs.LocHeader, errs.ReasonChecksum,
		errs.WithValue(expected)).Because(errs.ErrChecksumMismatch)
}

// validate checks the input before any body streams. What a caller controls
// (the share type and the title) is reported as violations, together
// (SPEC-0019 VE-4). The rest is server-derived, so a gap there is a bug in the
// adapter, never the caller's fault: it is an internal error, not a client
// violation.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-4; SPEC-0002 REQ "Artifact Aggregate
// and Invariants"
func (in CreateArtifactInput) validate(reg *sharetype.Registry) error {
	switch {
	case in.Provenance.Channel == "":
		return errors.New("create: provenance channel is required")
	case in.Provenance.ActorID == "":
		return errors.New("create: provenance actor is required")
	case in.Access.OwnerID == "":
		return errors.New("create: access owner is required")
	case in.Access.Visibility == "":
		return errors.New("create: access visibility is required")
	case in.ExpiresAt.IsZero():
		return errors.New("create: expiry is required")
	case in.Body == nil:
		return errors.New("create: nil body")
	}
	// A client-supplied bundle type is refused (not_allowed): a bundle is a
	// members-only type built via CreateBundle (NULL body + bundle_members), and
	// accepting it here would mint a malformed bundle (a body blob, no
	// members). SPEC-0002 REQ "Bundles with N Members".
	return errs.JoinErrors(
		reg.CheckCreateType(in.ShareType, "type", errs.LocBody),
		artifact.CheckTitle(in.Title, "title", errs.LocBody),
	)
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
	if err := in.validate(s.registry); err != nil {
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
	staged, err := StageBlob(ctx, s.obj, in.Body, s.maxBytes, in.DeclaredMediaType)
	if err != nil {
		return nil, fmt.Errorf("create: stream body: %w", err)
	}
	// The staging object is transient on EVERY path (committed OR rolled back);
	// reclaim it unconditionally so no staging/<rand> orphan the reaper cannot
	// reach survives. WithoutCancel is handled inside Discard.
	//
	// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
	defer staged.Discard(ctx, s.obj)

	// 2. Verify a client-declared checksum, if one was provided. The received
	//    body's digest stays out of the error, which is logged: for a body that
	//    is only a credential it is a fingerprint of that credential (SPEC-0017
	//    RD-9). The caller's own declared digest is echoed back to it (VE-3).
	if in.ExpectedSHA256 != "" && !strings.EqualFold(in.ExpectedSHA256, staged.SHA256) {
		return nil, fmt.Errorf("create: declared checksum does not match the received body: %w",
			ChecksumMismatch(in.ExpectedSHA256))
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
		Title:       in.Title,
		BodySHA256:  staged.SHA256,
		Size:        staged.Size,
		MediaType:   staged.MediaType,
		Previewable: previewable,
		Provenance:  in.Provenance,
		Access:      in.Access,
		Tags:        tags,
		ExpiresAt:   in.ExpiresAt,
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
			 owner_id, visibility, expires_at, tags)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id, created_at`

	tags := tagsParam(art.Tags)

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
			art.ExpiresAt, tags,
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
