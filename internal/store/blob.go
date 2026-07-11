package store

import (
	"context"
	"fmt"
	"io"

	"github.com/joestump/cairn/internal/objectstore"
)

// BlobResult describes a blob written to content-addressed object storage: the
// values a caller upserts into the `blobs` registry row inside its own
// transaction.
type BlobResult struct {
	SHA256     string
	Size       int64
	MediaType  string
	StorageKey string
}

// PutBlob streams r into content-addressed object storage, computing its
// SHA-256 incrementally, enforcing maxBytes as bytes arrive (errs.ErrTooLarge
// past the cap), sniffing the media type, promoting the bytes to their sharded
// content-addressed key unless that object already exists (object-level dedup —
// identical bytes are never re-uploaded), and removing the transient staging
// object on every path.
//
// It does NOT touch the database: the caller upserts the `blobs` registry row
// (`ON CONFLICT (sha256) DO NOTHING`) within its own transaction. This is the
// exact content-addressed ingest path artifact bodies use (it reuses the same
// streamBlob core and sharded key scheme), reused verbatim so a trajectory
// span's oversized output lands as one deduplicated blob just like a body.
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Content Addressing and Blobs",
// SPEC-0004 REQ "Span Output Storage and Content Addressing"
func PutBlob(ctx context.Context, obj objectstore.ObjectStore, r io.Reader, maxBytes int64, declaredMedia string) (BlobResult, error) {
	staged, err := streamBlob(ctx, obj, r, maxBytes, declaredMedia)
	if err != nil {
		return BlobResult{}, err
	}
	// The staging object is transient on every path (promoted by Copy on a new
	// blob, never promoted on a dedup hit); remove it unconditionally, with the
	// cancel stripped so cleanup still runs if the request context was cancelled.
	defer func() {
		_ = obj.Remove(context.WithoutCancel(ctx), staged.stagingKey)
	}()

	finalKey := shardedKey(staged.sha256)
	exists, err := obj.Stat(ctx, finalKey)
	if err != nil {
		return BlobResult{}, fmt.Errorf("put blob: stat object %s: %w", staged.sha256, err)
	}
	if !exists {
		if err := obj.Copy(ctx, staged.stagingKey, finalKey); err != nil {
			return BlobResult{}, fmt.Errorf("put blob: promote object %s: %w", staged.sha256, err)
		}
	}
	return BlobResult{
		SHA256:     staged.sha256,
		Size:       staged.size,
		MediaType:  staged.mediaType,
		StorageKey: finalKey,
	}, nil
}
