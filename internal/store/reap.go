package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// rowQuerier is the QueryRow surface shared by *pgxpool.Pool and pgx.Tx, so the
// cross-table refcount check runs identically inside or outside a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// blobStillReferenced reports whether any LIVE row across the four
// blob-bearing tables still points at this content hash: an artifact body, a
// bundle member, a trajectory span output, or a captured webhook body. It is the
// single canonical refcount predicate — the reference-counted GC rule of
// ADR-0008 — used by both the interactive delete and the SPEC-0009 reaper so
// they can never disagree about whether a blob is an orphan.
//
// Governing: ADR-0008 (content-addressed dedup across artifacts, bundle
// members, spans, hook_requests), SPEC-0009 REQ "Hard Delete on Expiry via
// Reference-Counted Reaper".
func blobStillReferenced(ctx context.Context, q rowQuerier, sha string) (bool, error) {
	var referenced bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM artifacts       WHERE body_sha256      = $1)
		     OR EXISTS(SELECT 1 FROM bundle_members  WHERE blob_sha256      = $1)
		     OR EXISTS(SELECT 1 FROM spans           WHERE output_ref_sha256 = $1)
		     OR EXISTS(SELECT 1 FROM hook_requests   WHERE body_ref_sha256   = $1)`,
		sha,
	).Scan(&referenced)
	if err != nil {
		return false, err
	}
	return referenced, nil
}

// ReaperConfig tunes one reaper sweep. Zero values fall back to safe defaults.
type ReaperConfig struct {
	// Batch bounds every phase: at most Batch expired artifacts per delete
	// transaction, Batch blob rows per unreferenced-blob scan, and Batch objects
	// per orphan-object scan. It keeps a single cycle's work (and lock hold time)
	// bounded regardless of backlog (SPEC-0009 REQ "Database Operation
	// Standards"). Default 500.
	Batch int
	// MaxBatches caps how many Batch-sized delete passes a single cycle drains,
	// so a huge expiry backlog is worked down over several cycles rather than in
	// one unbounded run. Default 20.
	MaxBatches int
	// ObjectGrace is the minimum age an object under blobs/ must reach before the
	// orphan-OBJECT scan (the no-DB-row backstop) may delete it. It is
	// defense-in-depth ONLY: it bounds the tiny window in which a create has
	// copied its object but not yet committed its (still-invisible) blob row, so
	// the scan never deletes a body an in-flight create is about to reference.
	// The primary guard is the blob-row lock, not this grace. It does NOT need to
	// exceed the max artifact TTL: the scan only ever removes objects that have NO
	// blob row, and a live body ALWAYS has a blob row (FK), so a referenced body
	// is structurally unreachable here (SPEC-0009 REQ "Object-Storage Lifecycle
	// Backstop", reinterpreted as an orphan scan per issue #93 §1). Default 1h.
	ObjectGrace time.Duration
}

func (c ReaperConfig) withDefaults() ReaperConfig {
	if c.Batch <= 0 {
		c.Batch = 500
	}
	if c.MaxBatches <= 0 {
		c.MaxBatches = 20
	}
	if c.ObjectGrace <= 0 {
		c.ObjectGrace = time.Hour
	}
	return c
}

// ReaperStats reports one sweep's work for metrics/logging (never secrets).
type ReaperStats struct {
	ArtifactsDeleted int
	BlobsGCd         int
	FreedBytes       int64
	OrphanObjects    int
}

// ReapOnce runs a single bounded retention sweep and returns what it reclaimed:
//
//  1. hard-delete artifacts past expires_at (their annotations, provenance,
//     bundle members, runs/spans, hooks/requests all cascade via FK);
//  2. reference-count every now-unreferenced blob and, under the blob-row lock,
//     delete its object then its `blobs` row (the committed-blob backstop is
//     this refcount orphan sweep, NEVER an age rule — issue #93 §1);
//  3. an object-storage orphan scan that removes blobs/ objects with no DB row
//     at all (rolled-back-create debris, failed-delete leftovers) past a grace
//     window.
//
// It is idempotent: a re-run with nothing to do reclaims nothing and errors
// nowhere. Each phase is independently bounded by cfg.Batch.
//
// Governing: SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted
// Reaper", REQ "Object-Storage Lifecycle Backstop", REQ "Concurrency Safety
// (Expiry Reaper)", REQ "Database Operation Standards"; ADR-0007 (default
// expiry), ADR-0008 (content-addressed dedup).
func (s *Store) ReapOnce(ctx context.Context, cfg ReaperConfig) (ReaperStats, error) {
	cfg = cfg.withDefaults()
	var stats ReaperStats

	// Phase 1: drain expired artifacts in bounded batches.
	for i := 0; i < cfg.MaxBatches; i++ {
		n, err := s.reapExpiredArtifacts(ctx, cfg.Batch)
		if err != nil {
			return stats, err
		}
		stats.ArtifactsDeleted += n
		if n < cfg.Batch {
			break // backlog drained for this cycle
		}
	}

	// Phase 2: GC blobs whose refcount just reached zero (and any left by a
	// prior failed delete), draining in bounded passes so a single sweep fully
	// reconciles when it can and a subsequent sweep with nothing to do is a
	// genuine no-op (idempotent).
	for i := 0; i < cfg.MaxBatches; i++ {
		scanned, gcd, freed, err := s.sweepUnreferencedBlobs(ctx, cfg.Batch)
		if err != nil {
			return stats, err
		}
		stats.BlobsGCd += gcd
		stats.FreedBytes += freed
		if scanned < cfg.Batch {
			break // fewer candidates than a full batch: backlog drained
		}
	}

	// Phase 3: object-storage backstop — remove blobs/ objects that have no DB
	// row at all, past the grace window. Never touches a referenced body.
	orphans, err := s.sweepOrphanObjects(ctx, cfg.Batch, cfg.ObjectGrace)
	if err != nil {
		return stats, err
	}
	stats.OrphanObjects = orphans

	return stats, nil
}

// reapExpiredArtifacts hard-deletes up to batch artifacts past expires_at in one
// transaction. FOR UPDATE SKIP LOCKED lets concurrent reapers (or a second
// cairnd) partition the work without blocking each other. All dependent rows —
// annotations, provenance (inline), bundle members, runs+spans, hooks+requests —
// cascade via ON DELETE CASCADE; the freed blobs are collected by phase 2.
//
// Governing: SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted Reaper"
// (scenario "Expired artifact reaped"), REQ "Atomic reap transaction".
func (s *Store) reapExpiredArtifacts(ctx context.Context, batch int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id FROM artifacts
		 WHERE expires_at <= now()
		 ORDER BY expires_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`,
		batch,
	)
	if err != nil {
		return 0, fmt.Errorf("reap: select expired: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reap: scan expired id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reap: iterate expired: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx, `DELETE FROM artifacts WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("reap: delete expired artifacts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reap: commit expired: %w", err)
	}
	return len(ids), nil
}

// sweepUnreferencedBlobs deletes, per blob, its object then its `blobs` row for
// every blob whose cross-table refcount is zero — under FOR UPDATE on the blob
// row, rechecking the refcount is STILL zero inside the lock. Because the
// content-addressed create path (CommitBlob) also locks that row before it
// decides to skip re-uploading the object, the two serialize: a create that
// races this sweep either (a) already committed a live reference, so the recheck
// sees refcount > 0 and skips the delete, or (b) blocks until this sweep commits
// the deletion and then re-inserts a fresh row and re-promotes the object. Neither
// interleaving loses a live body.
//
// A failed object delete is logged by the caller path and left in place: the row
// is NOT dropped (the tx rolls back), so the blob stays a known orphan and is
// retried next cycle rather than being masked as success (SPEC-0009 REQ "Error
// Handling Standards": "A failed blob delete MUST be logged ... never masked as
// success").
//
// Governing: SPEC-0009 REQ "Hard Delete on Expiry via Reference-Counted Reaper"
// (scenario "Shared blob survives refcount"), REQ "Concurrency Safety (Expiry
// Reaper)" (scenario "Concurrent create races the reaper"); ADR-0008.
// It returns (candidates scanned, blobs GC'd, bytes freed): scanned lets the
// caller loop until a pass returns fewer than a full batch (backlog drained).
func (s *Store) sweepUnreferencedBlobs(ctx context.Context, batch int) (int, int, int64, error) {
	// Candidate scan (no locks): blob rows with zero live references anywhere.
	rows, err := s.pool.Query(ctx,
		`SELECT sha256 FROM blobs b
		 WHERE NOT EXISTS (SELECT 1 FROM artifacts      WHERE body_sha256      = b.sha256)
		   AND NOT EXISTS (SELECT 1 FROM bundle_members WHERE blob_sha256      = b.sha256)
		   AND NOT EXISTS (SELECT 1 FROM spans          WHERE output_ref_sha256 = b.sha256)
		   AND NOT EXISTS (SELECT 1 FROM hook_requests  WHERE body_ref_sha256   = b.sha256)
		 LIMIT $1`,
		batch,
	)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reap: scan unreferenced blobs: %w", err)
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("reap: scan blob sha: %w", err)
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reap: iterate blobs: %w", err)
	}

	var (
		gcd   int
		freed int64
	)
	for _, sha := range shas {
		n, err := s.gcOneBlob(ctx, sha)
		if err != nil {
			return len(shas), gcd, freed, err
		}
		if n > 0 {
			gcd++
			freed += n
		}
	}
	return len(shas), gcd, freed, nil
}

// gcOneBlob deletes one orphan blob's object then row, atomically under the
// blob-row lock. It returns the freed byte size (0 if the blob was concurrently
// re-referenced or already gone). A failed object delete leaves the row intact
// for a later retry (never masked as success).
func (s *Store) gcOneBlob(ctx context.Context, sha string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap: begin blob gc: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the blob row. This is the serialization point against CommitBlob's
	// ON CONFLICT DO UPDATE (which also locks it): a concurrent create either
	// hasn't committed its reference yet (we win the lock; recheck below is the
	// authority) or already has (recheck sees it and we skip).
	var (
		storageKey string
		size       int64
	)
	err = tx.QueryRow(ctx,
		`SELECT storage_key, size_bytes FROM blobs WHERE sha256 = $1 FOR UPDATE`, sha,
	).Scan(&storageKey, &size)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil // already gone; nothing to do
		}
		return 0, fmt.Errorf("reap: lock blob %s: %w", sha, err)
	}

	// Re-check under the lock: a create may have referenced this hash between the
	// candidate scan and acquiring the lock. If so, it is NOT an orphan.
	referenced, err := blobStillReferenced(ctx, tx, sha)
	if err != nil {
		return 0, fmt.Errorf("reap: recheck refcount %s: %w", sha, err)
	}
	if referenced {
		return 0, nil // gained a live reference; leave it
	}

	// Delete the object BEFORE the row, still under the lock. On failure, leave
	// the row (roll back) so the blob remains a known orphan and is retried —
	// never reported as freed (SPEC-0009 error-handling standard).
	if err := s.obj.Remove(ctx, storageKey); err != nil {
		return 0, fmt.Errorf("reap: delete blob object %s (%s): %w", sha, storageKey, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM blobs WHERE sha256 = $1`, sha); err != nil {
		return 0, fmt.Errorf("reap: delete blob row %s: %w", sha, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reap: commit blob gc %s: %w", sha, err)
	}
	return size, nil
}

// sweepOrphanObjects is the object-storage backstop: it lists the committed
// blobs/ prefix and removes objects that have NO `blobs` row at all and are
// older than the grace window. These are upload debris (a create that copied its
// object then rolled back) or leftovers from a failed row delete. It is NOT an
// age rule on live bodies: a referenced body always has a blob row, so it is
// structurally excluded here; the grace window only bounds the brief window in
// which an in-flight create's just-copied object precedes its still-uncommitted
// (invisible) row (issue #93 §1: age rules on blobs/ are forbidden; this scan
// replaces them).
//
// Governing: SPEC-0009 REQ "Object-Storage Lifecycle Backstop" (scenario
// "Orphan swept"); ADR-0008.
func (s *Store) sweepOrphanObjects(ctx context.Context, batch int, grace time.Duration) (int, error) {
	objs, err := s.obj.List(ctx, "blobs/")
	if err != nil {
		return 0, fmt.Errorf("reap: list blobs objects: %w", err)
	}
	cutoff := time.Now().Add(-grace)
	removed := 0
	for _, o := range objs {
		if removed >= batch {
			break
		}
		if o.LastModified.After(cutoff) {
			continue // too fresh: could be an in-flight create's object
		}
		sha := shaFromKey(o.Key)
		if sha == "" {
			continue // not a content-addressed key we own
		}
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256 = $1)`, sha,
		).Scan(&exists); err != nil {
			return removed, fmt.Errorf("reap: check blob row %s: %w", sha, err)
		}
		if exists {
			continue // DB-known blob: age never deletes it (phase 2 owns it)
		}
		if err := s.obj.Remove(ctx, o.Key); err != nil {
			return removed, fmt.Errorf("reap: remove orphan object %s: %w", o.Key, err)
		}
		removed++
	}
	return removed, nil
}

// shaFromKey extracts the content hash from a sharded blobs/ key
// ("blobs/ab/cd/<sha>"); it returns "" for any key that is not that shape.
func shaFromKey(key string) string {
	if !strings.HasPrefix(key, "blobs/") {
		return ""
	}
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return ""
	}
	sha := key[i+1:]
	if len(sha) != 64 {
		return ""
	}
	return sha
}

// RunReaper runs ReapOnce on a ticker until ctx is cancelled, then returns — the
// explicit worker lifecycle SPEC-0009 requires. It runs one sweep immediately on
// start, logs each cycle's reclaimed counts (never secrets), and logs but does
// not abort on a sweep error (transient DB/object faults must not kill the
// worker; the next tick retries). A shutdown mid-sweep is a clean context
// cancellation: each phase's transaction rolls back rather than leaving a
// half-applied delete.
//
// Governing: SPEC-0009 REQ "Concurrency Safety (Expiry Reaper)" (scenarios
// "Graceful shutdown mid-sweep"), REQ "Metrics/logging".
func (s *Store) RunReaper(ctx context.Context, interval time.Duration, cfg ReaperConfig, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	logger.Info("retention reaper started", "interval", interval.String(), "batch", cfg.withDefaults().Batch)

	sweep := func() {
		stats, err := s.ReapOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; not a real failure
			}
			logger.Error("retention reaper sweep failed", "error", err)
			return
		}
		if stats.ArtifactsDeleted > 0 || stats.BlobsGCd > 0 || stats.OrphanObjects > 0 {
			logger.Info("retention reaper sweep",
				"artifacts_deleted", stats.ArtifactsDeleted,
				"blobs_gcd", stats.BlobsGCd,
				"freed_bytes", stats.FreedBytes,
				"orphan_objects", stats.OrphanObjects,
			)
		}
	}

	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("retention reaper stopped")
			return
		case <-ticker.C:
			sweep()
		}
	}
}
