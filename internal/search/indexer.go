package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

// ErrStopped reports that a drain stopped because its context was cancelled. A
// caller running a reindex treats it as "interrupted, re-run me", not as a
// broken index.
var ErrStopped = errors.New("search: indexer stopped")

// Config tunes the indexer worker pool. Zero values fall back to the defaults
// in withDefaults, which mirror the SPEC-0022 design's configuration table.
type Config struct {
	// Workers is the pool size (CAIRN_SEARCH_INDEX_WORKERS). It is bounded on
	// purpose: indexing reads bodies out of object storage and writes a
	// tsvector, and an unbounded pool turns a bulk import into a stampede on
	// both (SPEC-0022 "Concurrency Safety").
	Workers int
	// Batch is how many queue rows one worker claims per pass.
	Batch int
	// PollInterval is how long an idle worker waits before looking for due
	// work again. It only ever applies when the queue is empty: a worker
	// that finds work loops immediately, so a backlog drains at full speed.
	PollInterval time.Duration
	// MaxAttempts bounds a document's retries. Past it the queue row is dropped
	// and the failure logged: a permanently poison artifact must not hold the
	// pool's attention forever, and `cairn search reindex` is the repair path.
	MaxAttempts int
	// Backoff and BackoffMax give the retry delay: Backoff doubled per attempt,
	// capped at BackoffMax.
	Backoff    time.Duration
	BackoffMax time.Duration
	// Language is the Postgres text-search configuration for to_tsvector
	// (CAIRN_SEARCH_LANGUAGE, default english).
	Language string
	// MaxBodyBytes caps one body, and one bundle member, in the index
	// (CAIRN_SEARCH_MAX_BODY_BYTES, default 256 KiB).
	MaxBodyBytes int
	// Logger receives worker lifecycle and failure lines. Nil means
	// slog.Default().
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.Batch <= 0 {
		c.Batch = 50
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.Backoff <= 0 {
		c.Backoff = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 10 * time.Minute
	}
	if c.Language == "" {
		c.Language = "english"
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 256 << 10
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Blobs is the indexer's read surface on content-addressed storage.
// objectstore.ObjectStore satisfies it; the shape is what the indexer needs and
// nothing more, so tests hand it the in-memory store.
type Blobs interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// Indexer drains search_queue into search_documents with a bounded worker pool.
// It is stateless: all state is the two tables, so any number of Cairn
// processes may run an Indexer against one database and drain it together
// (FOR UPDATE SKIP LOCKED is the only coordination).
type Indexer struct {
	pool  *pgxpool.Pool
	blobs Blobs
	cfg   Config
	log   *slog.Logger
}

// New builds an indexer over the same pool the server serves from. blobs may be
// nil, in which case bodies are simply not read (title, tags and comments are
// still indexed) — that is a degenerate configuration, not a supported one, and
// nothing in the server does it.
func New(pool *pgxpool.Pool, blobs Blobs, cfg Config) *Indexer {
	cfg = cfg.withDefaults()
	return &Indexer{pool: pool, blobs: blobs, cfg: cfg, log: cfg.Logger.With("component", "search-indexer")}
}

// Run starts the worker pool and returns when ctx is cancelled and every
// worker has finished its current document. It is the reaper's contract
// (SPEC-0009 REQ "Concurrency Safety (Expiry Reaper)", SPEC-0012 REQ "Graceful
// Lifecycle"): no goroutine outlives Run, a cancellation mid-document finishes
// that document rather than abandoning it, and nothing is queued that the
// database does not already know about.
func (x *Indexer) Run(ctx context.Context) {
	x.log.Info("search indexer started", "workers", x.cfg.Workers, "batch", x.cfg.Batch, "language", x.cfg.Language)

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < x.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x.workerLoop(ctx)
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	<-done
	x.log.Info("search indexer stopped")
}

// workerLoop is one worker: claim, index, repeat; idle only when nothing is
// due. Cancelling ctx stops it between documents, never in the middle of one
// statement's transaction.
func (x *Indexer) workerLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := x.claim(ctx)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			// A claim failure is a database problem. Back off like a failed
			// document rather than spinning, and stay quiet about shutdown.
			x.log.Error("search indexer claim failed", "error", err)
			if !sleepCtx(ctx, x.cfg.PollInterval) {
				return
			}
			continue
		}
		if len(items) == 0 {
			if !sleepCtx(ctx, x.cfg.PollInterval) {
				return
			}
			continue
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			if err := x.indexOne(ctx, item); err != nil {
				x.reportFailure(ctx, item, err)
			}
		}
	}
}

// reportFailure re-queues a document with a longer delay, or drops it once it
// has used its retries. The dropped case is logged at Error: the artifact is
// live and searchable only by its absence, and `cairn search reindex` is how an
// operator fixes it.
func (x *Indexer) reportFailure(ctx context.Context, item queuedItem, err error) {
	attempts := item.attempts + 1
	if attempts >= x.cfg.MaxAttempts {
		x.log.Error("search indexer gave up on artifact",
			"artifact_id", item.artifactID, "attempts", attempts, "error", err)
		return
	}
	delay := x.backoff(attempts)
	if qerr := requeue(ctx, x.pool, item.artifactID, item.reason, attempts, int(delay.Seconds()), err.Error()); qerr != nil {
		x.log.Error("search indexer could not requeue artifact",
			"artifact_id", item.artifactID, "error", qerr)
		return
	}
	x.log.Warn("search indexer will retry artifact",
		"artifact_id", item.artifactID, "attempts", attempts,
		"retry_in", delay.String(), "error", err)
}

// backoff doubles per attempt and caps at BackoffMax.
func (x *Indexer) backoff(attempts int) time.Duration {
	d := x.cfg.Backoff
	for i := 1; i < attempts && d < x.cfg.BackoffMax; i++ {
		d *= 2
	}
	if d > x.cfg.BackoffMax {
		d = x.cfg.BackoffMax
	}
	return d
}

// sleepCtx waits d unless ctx is cancelled first; false means "shut down".
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// DrainStats reports one drain for the reindex command and tests (never any
// content).
type DrainStats struct {
	Indexed int
	// Missing counts documents skipped because their artifact was deleted or
	// expired while queued — the delete-during-index no-op, counted.
	Missing int
	// Failed counts documents whose index attempt errored and were re-queued or
	// dropped.
	Failed int
}

// Drain processes the queue to empty (or until maxBatches passes, or ctx ends)
// without a worker pool. It is what `cairn search reindex` runs after enqueueing
// so the command's exit means the index is rebuilt, and what tests use instead
// of racing a ticker. maxBatches <= 0 drains everything.
func (x *Indexer) Drain(ctx context.Context, maxBatches int) (DrainStats, error) {
	var stats DrainStats
	for pass := 0; maxBatches <= 0 || pass < maxBatches; pass++ {
		if ctx.Err() != nil {
			return stats, ErrStopped
		}
		items, err := x.claim(ctx)
		if err != nil {
			return stats, err
		}
		if len(items) == 0 {
			return stats, nil
		}
		for _, item := range items {
			switch err := x.indexOne(ctx, item); {
			case errors.Is(err, errArtifactGone):
				stats.Missing++
			case err != nil:
				stats.Failed++
				x.reportFailure(ctx, item, err)
			default:
				stats.Indexed++
			}
		}
	}
	return stats, nil
}

// queuedItem is one claimed unit of work. attempts is kept so a retry keeps
// backing off.
type queuedItem struct {
	artifactID int64
	reason     string
	attempts   int
}

// claimSQL takes a bounded batch of due rows and removes them from the queue in
// the same transaction that selected them, under FOR UPDATE SKIP LOCKED.
//
// Deleting on claim, rather than marking a row locked, is what lets the queue
// stay one row per artifact without a lock column: a worker holds the work, and
// a second event for the same artifact re-inserts a fresh row only after the
// first worker finishes (or dies, in which case the row reappears and the next
// pass does the work). Another worker never sees a claimed row, and never waits
// on one.
const claimSQL = `
	WITH claim AS (
		SELECT artifact_id, reason, attempts
		  FROM search_queue
		 WHERE not_before <= now()
		 ORDER BY not_before, artifact_id
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED
	), taken AS (
		DELETE FROM search_queue
		 WHERE (artifact_id) IN (SELECT artifact_id FROM claim)
		RETURNING artifact_id, reason, attempts
	)
	SELECT artifact_id, reason, attempts FROM taken
	ORDER BY artifact_id`

// claim removes and returns up to Batch due rows.
func (x *Indexer) claim(ctx context.Context) ([]queuedItem, error) {
	tx, err := x.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("search: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, claimSQL, x.cfg.Batch)
	if err != nil {
		return nil, fmt.Errorf("search: claim work: %w", err)
	}
	var items []queuedItem
	for rows.Next() {
		var item queuedItem
		if err := rows.Scan(&item.artifactID, &item.reason, &item.attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("search: scan claimed work: %w", err)
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: claim rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("search: commit claim: %w", err)
	}
	return items, nil
}

// errArtifactGone marks the delete-during-index no-op: the artifact row was
// already gone, or had expired, so there is nothing to index and nothing to
// fail over.
var errArtifactGone = errors.New("search: artifact no longer indexable")

// IndexOne builds and writes one artifact's document now. Tests and the
// reindex path use it directly; the pool reaches it through Drain.
func (x *Indexer) IndexOne(ctx context.Context, artifactID int64) error {
	doc, err := x.load(ctx, artifactID)
	if err != nil {
		return err
	}
	if doc == nil {
		return errArtifactGone
	}
	return x.write(ctx, doc)
}

// indexOne is IndexOne for a claimed queue row.
func (x *Indexer) indexOne(ctx context.Context, item queuedItem) error {
	return x.IndexOne(ctx, item.artifactID)
}

// document is everything one search_documents row needs.
type document struct {
	artifactID  int64
	bodyText    string
	commentText string
	content     string
}

// upsertSQL writes the document in one statement, in one transaction: the
// artifact row is the source of the denormalized filter columns (so the copy can
// never disagree with the artifact it indexes), and the weighted tsvector is
// built by Postgres so tokenization stays the database's job.
//
// Two properties matter for SPEC-0022:
//
//   - the SELECT reads artifacts, so an artifact deleted or expired since the
//     work was claimed produces no rows and therefore neither an insert nor an
//     update — delete-during-index is a no-op, and REQ-10's cascade has already
//     removed any earlier row;
//   - everything user-influenced is a bind parameter, including the text-search
//     configuration, so no artifact text or language name reaches the statement
//     as SQL ("Database Operation Standards").
const upsertSQL = `
	INSERT INTO search_documents
		(artifact_id, owner_user_id, owner_team_id, share_type, tags, "schema",
		 created_at, expires_at, doc, content, indexed_at)
	SELECT a.id, a.owner_user_id, a.owner_team_id, a.share_type, a.tags, $2::text,
	       a.created_at, a.expires_at,
	         setweight(to_tsvector($3::regconfig, coalesce(a.title, '')), 'A')
	       || setweight(to_tsvector($3::regconfig, array_to_string(a.tags, ' ')), 'B')
	       || setweight(to_tsvector($3::regconfig, coalesce($4, '')), 'C')
	       || setweight(to_tsvector($3::regconfig, coalesce($5, '')), 'D'),
	       $6, now()
	  FROM artifacts a
	 WHERE a.id = $1 AND a.expires_at > now()
	ON CONFLICT (artifact_id) DO UPDATE SET
		owner_user_id = EXCLUDED.owner_user_id,
		owner_team_id = EXCLUDED.owner_team_id,
		share_type    = EXCLUDED.share_type,
		tags          = EXCLUDED.tags,
		"schema"      = EXCLUDED."schema",
		created_at    = EXCLUDED.created_at,
		expires_at    = EXCLUDED.expires_at,
		doc           = EXCLUDED.doc,
		content       = EXCLUDED.content,
		indexed_at    = EXCLUDED.indexed_at`

func (x *Indexer) write(ctx context.Context, doc *document) error {
	_, err := x.pool.Exec(ctx, upsertSQL,
		doc.artifactID, nil, x.cfg.Language, doc.bodyText, doc.commentText, doc.content)
	if err != nil {
		return fmt.Errorf("search: write document for artifact %d: %w", doc.artifactID, err)
	}
	return nil
}

// load gathers one artifact's indexed text: its body (or, for a bundle, its
// text members) and its live comments. The title and tags need no work here:
// upsertSQL reads them off the artifact row itself.
func (x *Indexer) load(ctx context.Context, artifactID int64) (*document, error) {
	var (
		shareType string
		mediaType string
		bodySHA   *string
		expiresAt time.Time
	)
	err := x.pool.QueryRow(ctx, `
		SELECT share_type, media_type, body_sha256, expires_at
		  FROM artifacts WHERE id = $1`, artifactID).
		Scan(&shareType, &mediaType, &bodySHA, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search: load artifact %d: %w", artifactID, err)
	}
	if time.Now().After(expiresAt) {
		// Expired: invisible to every search anyway, and the reaper is on its
		// way. Counting it as "gone" keeps the drain stats honest.
		return nil, nil
	}

	var body strings.Builder
	if artifact.ShareType(shareType) == artifact.TypeBundle {
		if err := x.appendBundleMembers(ctx, &body, artifactID); err != nil {
			return nil, err
		}
	} else if bodySHA != nil && indexesBody(artifact.ShareType(shareType), mediaType) {
		text, err := x.readBlob(ctx, *bodySHA)
		if err != nil {
			return nil, err
		}
		body.WriteString(text)
	}

	comments, err := x.commentText(ctx, artifactID)
	if err != nil {
		return nil, err
	}

	content := body.String()
	if comments != "" {
		if content != "" {
			content += "\n\n"
		}
		content += comments
	}
	return &document{
		artifactID:  artifactID,
		bodyText:    body.String(),
		commentText: comments,
		content:     cappedText(content, x.cfg.MaxBodyBytes+maxCommentTextBytes),
	}, nil
}

// readBlob fetches one content-addressed body and returns at most MaxBodyBytes
// of it as text. A blob row or object that has vanished (a concurrent reaper)
// is not an error: the artifact's metadata is still indexed.
func (x *Indexer) readBlob(ctx context.Context, sha string) (string, error) {
	if x.blobs == nil {
		return "", nil
	}
	var key string
	err := x.pool.QueryRow(ctx, `SELECT storage_key FROM blobs WHERE sha256 = $1`, sha).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("search: blob %s: %w", sha, err)
	}
	rc, err := x.blobs.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("search: open blob %s: %w", sha, err)
	}
	defer func() { _ = rc.Close() }()

	buf, err := io.ReadAll(io.LimitReader(rc, int64(x.cfg.MaxBodyBytes)))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("search: read blob %s: %w", sha, err)
	}
	return cappedText(string(buf), x.cfg.MaxBodyBytes), nil
}

// appendBundleMembers concatenates a bundle's text members, each under the same
// per-body cap (SPEC-0022 REQ-2). Member order is the bundle's own ordinal
// order, so re-indexing the same bundle produces the same document.
func (x *Indexer) appendBundleMembers(ctx context.Context, out *strings.Builder, bundleID int64) error {
	rows, err := x.pool.Query(ctx, `
		SELECT m.blob_sha256, m.name, m.media_type
		  FROM bundle_members m
		 WHERE m.bundle_id = $1
		 ORDER BY m.ordinal`, bundleID)
	if err != nil {
		return fmt.Errorf("search: list bundle %d members: %w", bundleID, err)
	}
	type member struct {
		sha       string
		name      string
		mediaType string
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.sha, &m.name, &m.mediaType); err != nil {
			rows.Close()
			return fmt.Errorf("search: scan bundle %d members: %w", bundleID, err)
		}
		members = append(members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("search: bundle %d member rows: %w", bundleID, err)
	}

	for _, m := range members {
		if !memberIsText(m.mediaType) {
			continue
		}
		text, err := x.readBlob(ctx, m.sha)
		if err != nil {
			return err
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(m.name)
		out.WriteString("\n")
		out.WriteString(text)
	}
	return nil
}

// commentText concatenates the live comments on an artifact. Soft-deleted
// comments are excluded: SPEC-0022 REQ-2 indexes "the bodies of live,
// non-deleted comments", which is why a delete re-indexes rather than appending.
func (x *Indexer) commentText(ctx context.Context, artifactID int64) (string, error) {
	rows, err := x.pool.Query(ctx, `
		SELECT body FROM comments
		 WHERE artifact_id = $1 AND deleted_at IS NULL
		 ORDER BY id`, artifactID)
	if err != nil {
		return "", fmt.Errorf("search: list comments for artifact %d: %w", artifactID, err)
	}
	var out strings.Builder
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return "", fmt.Errorf("search: scan comments for artifact %d: %w", artifactID, err)
		}
		if out.Len() >= maxCommentTextBytes {
			break
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(body)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("search: comment rows for artifact %d: %w", artifactID, err)
	}
	return cappedText(out.String(), maxCommentTextBytes), nil
}
