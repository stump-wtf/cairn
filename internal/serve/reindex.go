// The reindex half of `cairn search reindex` (SPEC-0022 REQ-11): rebuild the
// lexical index from what is in the database and object storage right now.
//
// It is a server-side operator command, like `cairn serve` itself: it reads the
// same CAIRN_* environment and talks to Postgres and object storage directly.
// The CLI's REST-client boundary (SPEC-0008) still holds for every user-facing
// command; a rebuild is not a request any client should be able to make.
//
// Governing: ADR-0028, SPEC-0022 REQ-11 "Reindex and Model Changes" (lexical
// part), REQ-10 (an expired artifact is never indexed).
package serve

import (
	"context"
	"fmt"
	"io"

	"github.com/stump-wtf/cairn/internal/config"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/search"
)

// Reindex enqueues every live artifact — or, when owner is non-empty, every
// live artifact owned by that user or team — and drains the queue in this
// process, so the command's exit means the index is rebuilt rather than merely
// scheduled. It is safe to run while the server is up: the queue is claimed with
// FOR UPDATE SKIP LOCKED, so the two drain it together instead of duplicating
// work.
//
// Expired artifacts are excluded on purpose. Indexing one would put text into a
// row no search may return (REQ-10) and would keep that text around until the
// reaper arrives.
func Reindex(ctx context.Context, owner string, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrate first: `cairn search reindex` after a binary upgrade must not fail
	// because the tables the new binary owns are not there yet. Migrate is
	// advisory-locked and idempotent, so it is harmless when the server already
	// applied them.
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	obj, err := objectstore.NewMinIO(ctx, objectstore.MinIOConfig{
		Endpoint:  cfg.S3Endpoint,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Bucket:    cfg.S3Bucket,
		Region:    cfg.S3Region,
		UseSSL:    cfg.S3UseSSL,
	})
	if err != nil {
		return err
	}

	indexer := search.New(pool, obj, search.Config{
		Workers:      cfg.SearchIndexWorkers,
		Language:     cfg.SearchLanguage,
		MaxBodyBytes: cfg.SearchMaxBodyBytes,
	})

	if owner != "" {
		fmt.Fprintf(out, "reindexing live artifacts owned by %s\n", owner)
	} else {
		fmt.Fprintln(out, "reindexing all live artifacts")
	}

	enqueued, err := search.EnqueueLive(ctx, pool, owner)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "queued %d artifact(s)\n", enqueued)

	stats, err := indexer.Drain(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "indexed %d, skipped %d (deleted or expired), failed %d\n",
		stats.Indexed, stats.Missing, stats.Failed)
	if stats.Failed > 0 {
		return fmt.Errorf("search: reindex left %d artifact(s) unindexed; see the server log and re-run", stats.Failed)
	}
	return nil
}
