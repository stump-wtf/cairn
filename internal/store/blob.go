package store

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/cairn/internal/objectstore"
)

// StagedBlob is a body streamed into a transient staging object but NOT yet
// promoted to its content-addressed key and NOT yet registered in the `blobs`
// table. It is the first phase of the two-phase, reaper-safe blob write:
//
//  1. StageBlob streams the bytes (hash + size + sniffed media) to staging/.
//  2. CommitBlob, inside the caller's transaction, locks/inserts the `blobs`
//     row and promotes (or re-promotes) the object to blobs/ while holding that
//     row lock.
//
// Splitting the write this way is what serializes ingest against the SPEC-0009
// retention reaper: the object promotion happens under the same blob-row lock
// the reaper's orphan sweep takes FOR UPDATE, so the reaper can never delete a
// blob object between an ingest's "object exists" check and its reference
// commit (ADR-0008 content-addressed dedup; the create-vs-reaper TOCTOU).
type StagedBlob struct {
	SHA256     string
	Size       int64
	MediaType  string
	StorageKey string // content-addressed blobs/ key it commits to
	stagingKey string // transient staging/ key holding the bytes until commit
}

// StageBlob streams r into a transient staging object, computing its SHA-256
// incrementally, enforcing maxBytes as bytes arrive (errs.ErrTooLarge past the
// cap), and sniffing the media type. It does NOT promote the bytes to their
// content-addressed key and does NOT touch the database — the caller finalizes
// with CommitBlob inside its own transaction, and MUST Discard the staged blob
// on every path (defer) to reclaim the staging object.
//
// This is the exact content-addressed ingest core artifact bodies use, reused
// verbatim so a trajectory span's oversized output and a webhook's captured body
// land as deduplicated blobs just like a body.
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Content Addressing and Blobs",
// SPEC-0004 REQ "Span Output Storage and Content Addressing",
// SPEC-0009 REQ "Concurrency Safety (Expiry Reaper)".
func StageBlob(ctx context.Context, obj objectstore.ObjectStore, r io.Reader, maxBytes int64, declaredMedia string) (*StagedBlob, error) {
	staged, err := streamBlob(ctx, obj, r, maxBytes, declaredMedia)
	if err != nil {
		return nil, err
	}
	return &StagedBlob{
		SHA256:     staged.sha256,
		Size:       staged.size,
		MediaType:  staged.mediaType,
		StorageKey: shardedKey(staged.sha256),
		stagingKey: staged.stagingKey,
	}, nil
}

// Discard removes the transient staging object. It is a no-op if the blob was
// already promoted (Copy leaves the source in place; the reclaim is the same).
// Callers defer this on every path so a committed OR rolled-back ingest never
// leaks a staging/<rand> object the reaper cannot reclaim (staging keys are not
// in `blobs`). WithoutCancel so cleanup still runs when the request context was
// cancelled after commit.
//
// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
func (b *StagedBlob) Discard(ctx context.Context, obj objectstore.ObjectStore) {
	if b == nil {
		return
	}
	_ = obj.Remove(context.WithoutCancel(ctx), b.stagingKey)
}

// CommitBlob registers a staged blob's `blobs` row on tx and ensures its
// content-addressed object is present, all while holding the blob-row lock —
// the single serialization point against the SPEC-0009 reaper.
//
// It upserts with ON CONFLICT (sha256) DO UPDATE (a no-op touch), NOT DO
// NOTHING: DO UPDATE takes a row lock on the pre-existing blob row, whereas DO
// NOTHING takes none. That lock makes ingest and the reaper's orphan sweep
// (which SELECT ... FOR UPDATE the same row before deleting) mutually exclusive,
// so the reaper can never delete an object out from under an about-to-commit
// reference. RETURNING (xmax = 0) reports whether the row was freshly inserted.
//
// Object promotion, under that lock:
//   - fresh row (xmax = 0): the row was ABSENT, which means the reaper may have
//     just deleted both the row and its object — so we MUST re-promote the object
//     and never skip on a stale Stat. (§2 of the retention design.)
//   - pre-existing row: promote only if the object is missing (normal dedup —
//     identical bytes are never re-uploaded).
//
// Because promotion happens on tx while the row is locked, it is impossible for
// the reaper to observe refcount == 0 and delete the object between this check
// and the caller's reference INSERT: the reaper blocks on the row lock until
// this transaction commits, by which point a live reference exists.
//
// Governing: ADR-0008 (reference-counted GC; delayed reaper tolerates dedup
// races), SPEC-0009 REQ "Concurrency Safety (Expiry Reaper)" (scenario
// "Concurrent create races the reaper").
func CommitBlob(ctx context.Context, tx pgx.Tx, obj objectstore.ObjectStore, b *StagedBlob) error {
	var inserted bool
	if err := tx.QueryRow(ctx,
		`INSERT INTO blobs (sha256, size_bytes, media_type, storage_key)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (sha256) DO UPDATE SET media_type = blobs.media_type
		 RETURNING (xmax = 0)`,
		b.SHA256, b.Size, b.MediaType, b.StorageKey,
	).Scan(&inserted); err != nil {
		return fmt.Errorf("commit blob %s: %w", b.SHA256, err)
	}

	needPromote := inserted
	if !needPromote {
		exists, err := obj.Stat(ctx, b.StorageKey)
		if err != nil {
			return fmt.Errorf("commit blob %s: stat object: %w", b.SHA256, err)
		}
		needPromote = !exists
	}
	if needPromote {
		if err := obj.Copy(ctx, b.stagingKey, b.StorageKey); err != nil {
			return fmt.Errorf("commit blob %s: promote object: %w", b.SHA256, err)
		}
	}
	return nil
}
