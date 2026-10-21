// Package search is the lexical index layer of SPEC-0022: the queue write paths
// enqueue into, the indexer that drains it, and the backfill that rebuilds
// everything. The query side — ranking, snippets, the REST/MCP/CLI search
// surface — is the follow-up story and is deliberately absent here.
//
// The shape is ADR-0028's: Postgres full-text search over a `search_documents`
// table that lives and dies with its artifact by ON DELETE CASCADE, fed by a
// `search_queue` table that write paths populate AFTER their transaction
// commits. Indexing is therefore never on a create's critical path: an artifact
// is durable and served the moment its create commits, and it becomes
// searchable when a worker gets to it. With the indexer stopped, creates keep
// succeeding and the queue simply grows; on resume the backlog drains (SPEC-0022
// REQ-2).
//
// Text comes from two places, and only two: the artifact's stored body, which
// ADR-0023's ingest scanner already processed (SPEC-0022 REQ-8 — the indexer
// reads stored text, it never re-scans an original upload), and the artifact's
// live comments in Postgres. Binaries, images, webhook captures and trace span
// payloads are never indexed; a trace contributes its title and tags like any
// other artifact and nothing more.
//
// Governing: ADR-0028 (Search and Semantic Search), SPEC-0022 REQ-2 (indexed
// content), REQ-8 (redaction before indexing), REQ-10 (the index is deleted
// with the artifact), REQ-11 (reindex), "Concurrency Safety", "Database
// Operation Standards"; ADR-0008 (bodies live in object storage), ADR-0023 /
// SPEC-0017 (ingest redaction).
package search

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Reasons a document is queued. They are stored in search_queue.reason, whose
// CHECK constraint keeps this list and the schema's in step.
const (
	ReasonCreated  = "created"
	ReasonComment  = "comment"
	ReasonMetadata = "metadata"
	ReasonReindex  = "reindex"
)

// execer is the Exec surface of *pgxpool.Pool and pgx.Tx, so an enqueue runs
// either on the connection a write path just committed on or inside its
// transaction when a caller wants the two to be atomic.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// enqueueSQL puts one artifact in the queue by internal id.
//
// The insert is guarded by EXISTS on artifacts: an artifact deleted between the
// write path's commit and this enqueue must be a no-op rather than a foreign
// key error, because the enqueue runs after commit and can no longer be rolled
// back with the write.
//
// Folding into an existing row keeps the earliest not_before (so a queued item
// that is due is not pushed back by a second event) and preserves the attempt
// counter, so an artifact that keeps failing keeps backing off no matter how
// often it is touched.
const enqueueSQL = `
	INSERT INTO search_queue (artifact_id, reason, attempts, not_before, last_error)
	SELECT $1, $2, 0, now(), ''
	 WHERE EXISTS (SELECT 1 FROM artifacts WHERE id = $1)
	ON CONFLICT (artifact_id) DO UPDATE
	    SET reason     = EXCLUDED.reason,
	        not_before = LEAST(search_queue.not_before, EXCLUDED.not_before),
	        last_error = ''`

// enqueueByPublicIDSQL resolves the handle a transport adapter has (a public
// id, never the internal key — ADR-0005) and enqueues it.
const enqueueByPublicIDSQL = `
	INSERT INTO search_queue (artifact_id, reason, attempts, not_before, last_error)
	SELECT id, $2, 0, now(), ''
	  FROM artifacts
	 WHERE public_id = $1
	ON CONFLICT (artifact_id) DO UPDATE
	    SET reason     = EXCLUDED.reason,
	        not_before = LEAST(search_queue.not_before, EXCLUDED.not_before),
	        last_error = ''`

// EnqueueByID queues one artifact for (re)indexing. Call it AFTER the writing
// transaction commits: SPEC-0022 REQ-2 requires indexing to follow the commit,
// never to join it, so a queue write racing an uncommitted row could otherwise
// claim a document that does not exist yet.
//
// A missing artifact is not an error: the queue has nothing to index, and the
// caller's write already succeeded.
func EnqueueByID(ctx context.Context, ex execer, artifactID int64, reason string) error {
	if _, err := ex.Exec(ctx, enqueueSQL, artifactID, reason); err != nil {
		return fmt.Errorf("search: enqueue artifact %d: %w", artifactID, err)
	}
	return nil
}

// requeueSQL puts a failed artifact back with its attempt count bumped and its
// next claim delayed by the caller's backoff. The EXISTS guard is the same
// delete-during-index no-op: an artifact whose row is gone has no index to
// build and no queue row worth keeping (SPEC-0022 REQ-10).
const requeueSQL = `
	INSERT INTO search_queue (artifact_id, reason, attempts, not_before, last_error)
	SELECT $1, $2, $3, now() + make_interval(secs => $4::int), left($5, 512)
	 WHERE EXISTS (SELECT 1 FROM artifacts WHERE id = $1)
	ON CONFLICT (artifact_id) DO UPDATE
	    SET reason     = EXCLUDED.reason,
	        attempts   = EXCLUDED.attempts,
	        not_before = EXCLUDED.not_before,
	        last_error = EXCLUDED.last_error`

func requeue(ctx context.Context, ex execer, artifactID int64, reason string, attempts int, delaySecs int, lastError string) error {
	if _, err := ex.Exec(ctx, requeueSQL, artifactID, reason, attempts, delaySecs, lastError); err != nil {
		return fmt.Errorf("search: requeue artifact %d: %w", artifactID, err)
	}
	return nil
}

// Enqueuer is the slice of the indexer the write paths need. The store and the
// annotation service depend on this, never on *Indexer, so a server started
// without an indexer passes nil and behaves exactly as it did before the index
// existed.
type Enqueuer interface {
	EnqueueArtifact(ctx context.Context, artifactID int64, reason string) error
}

// EnqueueArtifact implements Enqueuer.
func (x *Indexer) EnqueueArtifact(ctx context.Context, artifactID int64, reason string) error {
	return EnqueueByID(ctx, x.pool, artifactID, reason)
}

// enqueueLiveQueue is the backfill's enqueue: every LIVE artifact, optionally
// narrowed to one owner, into the queue with reason 'reindex'.
//
// Expired artifacts are excluded on purpose (SPEC-0022 REQ-11: "live artifacts
// only"). They are already invisible and the reaper will remove them; indexing
// one would put text into the index that no query may return.
const enqueueLiveQueue = `
	INSERT INTO search_queue (artifact_id, reason, attempts, not_before, last_error)
	SELECT id, 'reindex', 0, now(), ''
	  FROM artifacts
	 WHERE expires_at > now()
	   AND ($1::text = '' OR COALESCE(owner_user_id::text, '') = $1 OR COALESCE(owner_team_id::text, '') = $1)
	ON CONFLICT (artifact_id) DO UPDATE
	    SET reason     = EXCLUDED.reason,
	        not_before = LEAST(search_queue.not_before, EXCLUDED.not_before),
	        last_error = ''`

// EnqueueLive queues every live artifact (or, when owner is non-empty, every
// live artifact whose workspace is that user or team id) for a rebuild. It is
// the enqueue half of `cairn search reindex`; the drain half is Indexer.Drain.
// It returns how many rows it touched.
func EnqueueLive(ctx context.Context, ex execer, owner string) (int64, error) {
	tag, err := ex.Exec(ctx, enqueueLiveQueue, owner)
	if err != nil {
		return 0, fmt.Errorf("search: enqueue live artifacts for reindex: %w", err)
	}
	return tag.RowsAffected(), nil
}

// compile-time proof that the indexer satisfies the write paths' seam.
var _ Enqueuer = (*Indexer)(nil)
