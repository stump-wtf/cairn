package trajectory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/id"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
)

// Default output-inlining threshold and per-span output cap. A span output at
// or below the threshold is stored inline on the row; a larger one spills to a
// content-addressed blob (SPEC-0004 "Span Output Storage").
const (
	defaultInlineThreshold = 16 << 10 // 16 KiB
	defaultMaxOutputBytes  = 64 << 20 // 64 MiB per span output
	idMaxAttempts          = 5
	mediaTypeTrajectory    = "application/vnd.cairn.trajectory"
)

// Service is the trajectory core service. It shares the store's Postgres pool
// and object store so a trajectory is an ordinary artifact in the same database
// and transaction domain (ADR-0012 one binary, one core).
type Service struct {
	pool            *pgxpool.Pool
	obj             objectstore.ObjectStore
	reg             *sharetype.Registry
	inlineThreshold int64
	maxOutputBytes  int64
	newID           func() (string, error)
	now             func() time.Time
	// hub fans appended spans and the close transition out to live SSE
	// subscribers of an open run. It is process-local: one binary owns the
	// capture, so its in-memory fan-out and the persisted span rows are the same
	// ordered log two transports read (SPEC-0004 "Live Span Stream Delivery").
	hub *hub
}

// Options configures a Service. Zero values fall back to safe defaults.
type Options struct {
	// InlineThresholdBytes is the size at or below which a span output is stored
	// inline; above it the output spills to a blob (default 16 KiB).
	InlineThresholdBytes int64
	// MaxOutputBytes caps a single span output (default 64 MiB); an output past
	// it is rejected mid-stream with payload_too_large.
	MaxOutputBytes int64
	// Registry gates the trajectory's anchor affordances; defaults to the
	// process-wide sharetype.Default().
	Registry *sharetype.Registry
	// NewID overrides public-id generation; tests inject forced collisions.
	NewID func() (string, error)
	// Now overrides the clock, for deterministic tests of live wall time.
	Now func() time.Time
}

// NewService constructs a Service over a Postgres pool and an object store.
func NewService(pool *pgxpool.Pool, obj objectstore.ObjectStore, opts Options) *Service {
	s := &Service{
		pool:            pool,
		obj:             obj,
		reg:             opts.Registry,
		inlineThreshold: opts.InlineThresholdBytes,
		maxOutputBytes:  opts.MaxOutputBytes,
		newID:           opts.NewID,
		now:             opts.Now,
		hub:             newHub(),
	}
	if s.reg == nil {
		s.reg = sharetype.Default()
	}
	if s.inlineThreshold <= 0 {
		s.inlineThreshold = defaultInlineThreshold
	}
	if s.maxOutputBytes <= 0 {
		s.maxOutputBytes = defaultMaxOutputBytes
	}
	if s.newID == nil {
		s.newID = id.New
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// spilled holds a span's output disposition after the inline-vs-blob decision.
// When staged is non-nil the output spilled to a content-addressed blob that is
// streamed to a staging object but NOT yet promoted or registered — persistSpans
// finalizes it via store.CommitBlob under the blob-row lock, and the caller
// Discards the staging object on every path (the reaper-safe two-phase write).
type spilled struct {
	inline    *string
	staged    *store.StagedBlob
	size      int64
	truncated bool
}

// refSHA is the output's content hash, or "" when it stayed inline or was empty.
func (d spilled) refSHA() string {
	if d.staged == nil {
		return ""
	}
	return d.staged.SHA256
}

// discardStaged reclaims the staging object of every spilled output on the given
// dispositions (committed OR rolled back) so no staging/<rand> orphan survives.
func discardStaged(ctx context.Context, obj objectstore.ObjectStore, ds []spilled) {
	for _, d := range ds {
		d.staged.Discard(ctx, obj)
	}
}

// spillOutputs decides, for each span, whether its output stays inline or spills
// to a content-addressed blob, writing every oversized output to object storage
// BEFORE the transaction opens (mirroring the artifact create path). It returns
// one disposition per input span, index-aligned. Object writes that outlive a
// later transaction rollback orphan only GC-collectable blobs (ADR-0008 reaper).
func (s *Service) spillOutputs(ctx context.Context, spans []SpanInput) ([]spilled, error) {
	out := make([]spilled, len(spans))
	for i, sp := range spans {
		size := int64(len(sp.Output))
		if size > s.maxOutputBytes {
			return nil, fmt.Errorf("trajectory: span %q output: %w", sp.SpanID, errs.ErrTooLarge)
		}
		if size == 0 {
			out[i] = spilled{truncated: sp.OutputTruncated}
			continue
		}
		if size <= s.inlineThreshold {
			text := string(sp.Output)
			out[i] = spilled{inline: &text, size: size, truncated: sp.OutputTruncated}
			continue
		}
		staged, err := store.StageBlob(ctx, s.obj, byteReader(sp.Output), s.maxOutputBytes, "")
		if err != nil {
			return nil, fmt.Errorf("trajectory: spill span %q output: %w", sp.SpanID, err)
		}
		out[i] = spilled{
			staged:    staged,
			size:      staged.Size,
			truncated: sp.OutputTruncated,
		}
	}
	return out, nil
}

// CreateBatchRun ingests a complete run in one call: it mints the artifact, the
// run row (status = closed, ended_at stamped from the span tree), the full span
// tree, and any produced edges — all in one transaction so a partial run is
// never visible (SPEC-0004 "Run Ingestion — Batch", "Database Operation
// Standards").
func (s *Service) CreateBatchRun(ctx context.Context, in RunInput) (*Run, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	dispositions, err := s.spillOutputs(ctx, in.Spans)
	if err != nil {
		return nil, err
	}
	defer discardStaged(ctx, s.obj, dispositions)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trajectory: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	artID, publicID, err := s.insertRunArtifact(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	// ended_at is stamped from the tree so batch and incremental converge.
	endedAt := in.StartedAt.Add(time.Duration(maxSpanEndMS(spanInputEnds(in.Spans))) * time.Millisecond)
	runID, err := s.insertRun(ctx, tx, artID, in, StatusClosed, &endedAt)
	if err != nil {
		return nil, err
	}

	prepared, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{}, in.Spans)
	if err != nil {
		return nil, err
	}
	// A fresh run's stream sequence starts at 0; its id is not handed out until
	// after commit, so no live subscriber can exist yet and none is published to.
	if err := s.persistSpans(ctx, tx, runID, 0, prepared, dispositions); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trajectory: commit: %w", err)
	}
	return s.GetRun(ctx, publicID)
}

// OpenRun opens a live run and returns its id and share URL immediately, before
// any span is appended, so a human can be handed a link to a run in flight. Any
// spans supplied up front are ingested in the same transaction (SPEC-0004 "Open
// returns a shareable link immediately").
func (s *Service) OpenRun(ctx context.Context, in RunInput) (*Run, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	dispositions, err := s.spillOutputs(ctx, in.Spans)
	if err != nil {
		return nil, err
	}
	defer discardStaged(ctx, s.obj, dispositions)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trajectory: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	artID, publicID, err := s.insertRunArtifact(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	runID, err := s.insertRun(ctx, tx, artID, in, StatusOpen, nil)
	if err != nil {
		return nil, err
	}

	if len(in.Spans) > 0 {
		prepared, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{}, in.Spans)
		if err != nil {
			return nil, err
		}
		// Seed spans start the sequence at 0 like a batch; the run id has not been
		// returned yet, so no live subscriber exists to publish to.
		if err := s.persistSpans(ctx, tx, runID, 0, prepared, dispositions); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trajectory: commit: %w", err)
	}
	return s.GetRun(ctx, publicID)
}

// AppendSpans appends one or more spans to an OPEN run. It is additive only:
// appending to a closed run is refused with the ErrRunClosed sentinel, and a
// non-owner is refused with ErrNotOwner. The run row is locked FOR UPDATE so
// concurrent appends serialize and each span gets a distinct, gap-free sibling
// seq (SPEC-0004 "Append to a closed run refused", "Concurrent appends keep seq
// monotonic").
func (s *Service) AppendSpans(ctx context.Context, publicID, actorID string, spans []SpanInput) ([]*Span, error) {
	if len(spans) == 0 {
		return nil, errs.Validationf("trajectory: no spans to append")
	}
	dispositions, err := s.spillOutputs(ctx, spans)
	if err != nil {
		return nil, err
	}
	defer discardStaged(ctx, s.obj, dispositions)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trajectory: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rr, err := s.lockRun(ctx, tx, publicID)
	if err != nil {
		return nil, err
	}
	if rr.ownerID != actorID {
		return nil, fmt.Errorf("trajectory: append by %q to run owned by %q: %w", actorID, rr.ownerID, ErrNotOwner)
	}
	if rr.status == StatusClosed {
		return nil, fmt.Errorf("trajectory: append to run %s: %w", publicID, ErrRunClosed)
	}

	existingDepth, existingChildCount, existingIDs, err := s.loadSpanShape(ctx, tx, rr.runID)
	if err != nil {
		return nil, err
	}

	// Idempotent span ingest (SPEC-0004 "Run Ingestion — Batch and Incremental":
	// appends are additive only). span_id is the natural idempotency key, so a
	// retried append of an already-acknowledged batch must neither duplicate a
	// span nor raise a spurious conflict. Three cases:
	//   - every span already present  -> a no-op replay: commit nothing, return
	//     the stored spans so an at-least-once client sees success;
	//   - some present, some new      -> a partial/inconsistent replay, rejected
	//     whole so the append stays all-or-nothing;
	//   - none present                -> an ordinary append (the common path).
	newCount := 0
	for _, sp := range spans {
		if !existingIDs[sp.SpanID] {
			newCount++
		}
	}
	if newCount == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("trajectory: commit: %w", err)
		}
		return s.spansByID(ctx, rr.runID, spans)
	}
	if newCount != len(spans) {
		return nil, errs.Validationf(
			"trajectory: append mixes %d new spans with already-present spans; resend the whole batch or only the new spans", newCount)
	}

	prepared, err := prepareSpans(existingDepth, existingChildCount, existingIDs, spans)
	if err != nil {
		return nil, err
	}
	base, err := s.nextStreamBase(ctx, tx, rr.runID)
	if err != nil {
		return nil, err
	}
	if err := s.persistSpans(ctx, tx, rr.runID, base, prepared, dispositions); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trajectory: commit: %w", err)
	}

	// Fan the durably-committed spans out to live viewers in ingest order,
	// each carrying its stream_seq cursor — the same id: a reconnecting client
	// resumes after (SPEC-0004 "Live Span Stream Delivery"). Published only
	// after commit so a viewer never sees a span a rolled-back tx would erase.
	appended := make([]*Span, 0, len(prepared))
	for i, p := range prepared {
		sp := spanFromPrepared(p, dispositions[i])
		s.hub.publish(publicID, StreamEvent{Type: EventSpan, StreamSeq: base + int64(i) + 1, Span: sp})
		appended = append(appended, sp)
	}
	return appended, nil
}

// CloseRun closes an open run, stamping ended_at from the span tree (so it
// matches a batch run) and freezing it against further spans. Only the owner may
// close it; a closed run stays closed (SPEC-0004 "Run Model and Lifecycle",
// "Non-owner cannot close a run").
func (s *Service) CloseRun(ctx context.Context, publicID, actorID string) (*Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trajectory: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rr, err := s.lockRun(ctx, tx, publicID)
	if err != nil {
		return nil, err
	}
	if rr.ownerID != actorID {
		return nil, fmt.Errorf("trajectory: close by %q of run owned by %q: %w", actorID, rr.ownerID, ErrNotOwner)
	}
	if rr.status == StatusClosed {
		return nil, fmt.Errorf("trajectory: close run %s: %w", publicID, ErrRunClosed)
	}

	var maxEnd int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(start_offset_ms + duration_ms), 0) FROM spans WHERE run_id = $1`, rr.runID,
	).Scan(&maxEnd); err != nil {
		return nil, fmt.Errorf("trajectory: max span end for %s: %w", publicID, err)
	}
	endedAt := rr.startedAt.Add(time.Duration(maxEnd) * time.Millisecond)
	if _, err := tx.Exec(ctx,
		`UPDATE runs SET status = 'closed', ended_at = $2 WHERE id = $1`, rr.runID, endedAt,
	); err != nil {
		return nil, fmt.Errorf("trajectory: close run %s: %w", publicID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("trajectory: commit: %w", err)
	}
	// Signal connected viewers the run is closed so their live badge flips and
	// their SSE stream ends cleanly (SPEC-0004 "open→live→closed").
	s.hub.publish(publicID, StreamEvent{Type: EventStatus, Status: StatusClosed})
	return s.GetRun(ctx, publicID)
}

// runRow is the locked run header used by append/close.
type runRow struct {
	runID     int64
	ownerID   string
	status    Status
	startedAt time.Time
}

// lockRun loads and row-locks a run header by public id, returning ErrRunNotFound
// uniformly for unknown or expired runs. FOR UPDATE serializes concurrent
// appends/closes so seq assignment and the append-after-close check are race-free.
func (s *Service) lockRun(ctx context.Context, tx pgx.Tx, publicID string) (runRow, error) {
	var rr runRow
	err := tx.QueryRow(ctx, `
		SELECT r.id, a.owner_id, r.status, r.started_at
		FROM runs r
		JOIN artifacts a ON a.id = r.artifact_id
		WHERE a.public_id = $1 AND a.expires_at > now()
		FOR UPDATE OF r`, publicID).Scan(&rr.runID, &rr.ownerID, &rr.status, &rr.startedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runRow{}, fmt.Errorf("trajectory: run %s: %w", publicID, ErrRunNotFound)
		}
		return runRow{}, fmt.Errorf("trajectory: lock run %s: %w", publicID, err)
	}
	return rr, nil
}

// loadSpanShape reads the persisted tree shape needed to validate and place an
// append: each span's depth, the child count under each parent (keyed "" for
// the top level), and the set of existing span_ids.
func (s *Service) loadSpanShape(ctx context.Context, tx pgx.Tx, runID int64) (map[string]int, map[string]int, map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT span_id, COALESCE(parent_span_id, ''), depth FROM spans WHERE run_id = $1`, runID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("trajectory: load span shape: %w", err)
	}
	defer rows.Close()

	depth := map[string]int{}
	childCount := map[string]int{}
	ids := map[string]bool{}
	for rows.Next() {
		var spanID, parent string
		var d int
		if err := rows.Scan(&spanID, &parent, &d); err != nil {
			return nil, nil, nil, fmt.Errorf("trajectory: scan span shape: %w", err)
		}
		depth[spanID] = d
		ids[spanID] = true
		childCount[parent]++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("trajectory: iterate span shape: %w", err)
	}
	return depth, childCount, ids, nil
}

// persistSpans upserts each spilled output blob and inserts each span row, then
// any produced edge, all on the caller's transaction. Each span is stamped a
// per-run stream_seq (baseStreamSeq + its index + 1) in ingest order, so the SSE
// endpoint has a gap-free, append-monotonic resume cursor (SPEC-0004 "Live Span
// Stream Delivery"). base is the run's current MAX(stream_seq) — 0 for a fresh
// run, the locked current max for an append — so appends continue the sequence.
func (s *Service) persistSpans(ctx context.Context, tx pgx.Tx, runID, baseStreamSeq int64, prepared []preparedSpan, dispositions []spilled) error {
	for i, p := range prepared {
		d := dispositions[i]
		if d.staged != nil {
			// Register + promote the span-output blob under its row lock, so this
			// write serializes against the reaper's orphan sweep (ADR-0008 / SPEC-0009).
			if err := store.CommitBlob(ctx, tx, s.obj, d.staged); err != nil {
				return fmt.Errorf("trajectory: %w", err)
			}
		}
		if err := s.insertSpan(ctx, tx, runID, baseStreamSeq+int64(i)+1, p, d); err != nil {
			return err
		}
		if p.in.ProducedArtifactID != "" {
			if err := s.insertProducedEdge(ctx, tx, runID, p.in.SpanID, p.in.ProducedArtifactID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) insertSpan(ctx context.Context, tx pgx.Tx, runID, streamSeq int64, p preparedSpan, d spilled) error {
	args := p.in.Args
	if len(args) == 0 {
		args = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO spans
			(run_id, span_id, parent_span_id, depth, seq, category, name, tool,
			 args, output_inline, output_ref_sha256, output_size, output_truncated,
			 start_offset_ms, duration_ms, stream_seq)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		runID, p.in.SpanID, nullString(p.in.ParentSpanID), p.depth, p.seq,
		string(p.in.Category), p.in.Name, nullString(p.in.Tool), args,
		d.inline, nullString(d.refSHA()), d.size, d.truncated,
		p.in.StartOffsetMS, p.in.DurationMS, streamSeq,
	); err != nil {
		return fmt.Errorf("trajectory: insert span %q: %w", p.in.SpanID, err)
	}
	return nil
}

// nextStreamBase returns the run's current MAX(stream_seq) so an append
// continues the per-run sequence. The caller holds the run row lock (FOR UPDATE),
// so concurrent appends read distinct, increasing bases and never collide
// (SPEC-0004 "Concurrent appends keep seq monotonic").
func (s *Service) nextStreamBase(ctx context.Context, tx pgx.Tx, runID int64) (int64, error) {
	var base int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(stream_seq), 0) FROM spans WHERE run_id = $1`, runID,
	).Scan(&base); err != nil {
		return 0, fmt.Errorf("trajectory: read stream base: %w", err)
	}
	return base, nil
}

// insertProducedEdge resolves the produced artifact's public id to its internal
// id (uniform not-found for unknown/expired) and records the directed edge. The
// id arrives already normalized from prepareSpans, so an mcp://cairn/<id> handle
// resolves exactly as the bare id does (issue #50).
func (s *Service) insertProducedEdge(ctx context.Context, tx pgx.Tx, runID int64, spanID, pid string) error {
	var artID int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM artifacts WHERE public_id = $1 AND expires_at > now()`, pid,
	).Scan(&artID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.Validationf("trajectory: span %q produced-artifact %q not found", spanID, pid)
		}
		return fmt.Errorf("trajectory: resolve produced-artifact %q: %w", pid, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO produced_edges (run_id, span_id, artifact_id)
		 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		runID, spanID, artID,
	); err != nil {
		return fmt.Errorf("trajectory: insert produced edge for span %q: %w", spanID, err)
	}
	return nil
}

// insertRunArtifact mints the trajectory artifact envelope (body-less, like a
// bundle), retrying id generation on the rare public_id collision via a
// savepoint. It reuses the artifact aggregate's Validate for the envelope
// invariants.
func (s *Service) insertRunArtifact(ctx context.Context, tx pgx.Tx, in RunInput) (int64, string, error) {
	art := &artifact.Artifact{
		ShareType:   artifact.TypeTrajectory,
		Title:       in.Title,
		MediaType:   mediaTypeTrajectory,
		Previewable: false,
		Provenance:  in.Provenance,
		Access:      in.Access,
		ExpiresAt:   in.ExpiresAt,
	}
	const insertSQL = `
		INSERT INTO artifacts
			(public_id, share_type, title, body_sha256, size_bytes, media_type,
			 previewable, actor_id, on_behalf_of, channel, captured_at,
			 owner_id, visibility, expires_at)
		VALUES ($1,$2,$3,NULL,0,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id`

	for attempt := 0; attempt < idMaxAttempts; attempt++ {
		pid, err := s.newID()
		if err != nil {
			return 0, "", fmt.Errorf("trajectory: generate id: %w", err)
		}
		art.PublicID = pid
		if err := art.Validate(); err != nil {
			return 0, "", err // invariant violation, identical every attempt
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return 0, "", fmt.Errorf("trajectory: savepoint: %w", err)
		}
		var artID int64
		err = sp.QueryRow(ctx, insertSQL,
			art.PublicID, string(art.ShareType), art.Title, art.MediaType,
			art.Previewable, art.Provenance.ActorID, art.Provenance.OnBehalfOf,
			string(art.Provenance.Channel), art.Provenance.CapturedAt,
			art.Access.OwnerID, string(art.Access.Visibility), art.ExpiresAt,
		).Scan(&artID)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isPublicIDConflict(err) {
				continue
			}
			return 0, "", fmt.Errorf("trajectory: insert run artifact: %w", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return 0, "", fmt.Errorf("trajectory: release savepoint: %w", err)
		}
		return artID, art.PublicID, nil
	}
	return 0, "", fmt.Errorf("trajectory: exhausted %d id attempts: %w", idMaxAttempts, errs.ErrConflict)
}

func (s *Service) insertRun(ctx context.Context, tx pgx.Tx, artID int64, in RunInput, status Status, endedAt *time.Time) (int64, error) {
	var runID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO runs (artifact_id, prompt, model, status, started_at, ended_at, token_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		artID, in.Prompt, in.Model, string(status), in.StartedAt, endedAt, in.TokenCount,
	).Scan(&runID)
	if err != nil {
		return 0, fmt.Errorf("trajectory: insert run: %w", err)
	}
	return runID, nil
}

// isPublicIDConflict reports whether err is a unique-violation on public_id.
func isPublicIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == "artifacts_public_id_key"
	}
	return false
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// spanInputEnds adapts SpanInputs to the minimal *Span slice maxSpanEndMS needs.
func spanInputEnds(spans []SpanInput) []*Span {
	out := make([]*Span, len(spans))
	for i, sp := range spans {
		out[i] = &Span{StartOffsetMS: sp.StartOffsetMS, DurationMS: sp.DurationMS}
	}
	return out
}

// spanFromPrepared projects a freshly appended span for the caller.
func spanFromPrepared(p preparedSpan, d spilled) *Span {
	sp := &Span{
		SpanID:        p.in.SpanID,
		ParentSpanID:  p.in.ParentSpanID,
		Depth:         p.depth,
		Seq:           p.seq,
		Category:      p.in.Category,
		Name:          p.in.Name,
		Tool:          p.in.Tool,
		Args:          p.in.Args,
		StartOffsetMS: p.in.StartOffsetMS,
		DurationMS:    p.in.DurationMS,
	}
	if d.inline != nil {
		sp.Inline = *d.inline
	}
	if d.refSHA() != "" {
		sp.Ref = &OutputRef{SHA256: d.refSHA(), Size: d.size, Truncated: d.truncated}
	}
	if p.in.ProducedArtifactID != "" {
		sp.ProducedArtifactIDs = []string{p.in.ProducedArtifactID}
	}
	return sp
}

// byteReader wraps bytes for streaming into the CAS without importing bytes at
// call sites everywhere.
func byteReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
