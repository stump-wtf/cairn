package trajectory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// GetRun resolves a run by its public id and returns the run header, its
// ordered span tree, and its derived stats. It returns ErrRunNotFound uniformly
// for unknown, unauthorized, or expired ids (ADR-0007 link-capability). Stats
// and the tree are computed from the same span rows, so the RUN panel and the
// waterfall can never disagree (SPEC-0004 "Derived Run Statistics").
func (s *Service) GetRun(ctx context.Context, publicID string) (*Run, error) {
	run, runID, err := s.loadRunHeader(ctx, publicID)
	if err != nil {
		return nil, err
	}

	flat, err := s.loadSpans(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := s.attachProducedEdges(ctx, runID, flat); err != nil {
		return nil, err
	}

	var wallMS int64
	if run.Status == StatusClosed && !run.EndedAt.IsZero() {
		wallMS = run.EndedAt.Sub(run.StartedAt).Milliseconds()
	} else {
		wallMS = s.now().Sub(run.StartedAt).Milliseconds()
	}
	if wallMS < 0 {
		wallMS = 0
	}

	run.Stats = computeStats(flat, run.TokenCount, wallMS)
	run.Spans = buildTree(flat)
	return run, nil
}

// loadRunHeader reads the run + its artifact envelope, enforcing the expiry
// (hard non-existence) link-capability policy.
func (s *Service) loadRunHeader(ctx context.Context, publicID string) (*Run, int64, error) {
	var (
		run     Run
		runID   int64
		endedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, a.public_id, a.title, r.prompt, r.model, r.status,
		       r.started_at, r.ended_at, r.token_count,
		       a.actor_id, a.on_behalf_of, a.channel, a.captured_at,
		       a.owner_id, a.visibility, a.expires_at
		FROM runs r
		JOIN artifacts a ON a.id = r.artifact_id
		WHERE a.public_id = $1 AND a.expires_at > now()`, publicID).Scan(
		&runID, &run.PublicID, &run.Title, &run.Prompt, &run.Model, &run.Status,
		&run.StartedAt, &endedAt, &run.TokenCount,
		&run.Provenance.ActorID, &run.Provenance.OnBehalfOf, &run.Provenance.Channel,
		&run.Provenance.CapturedAt, &run.Access.OwnerID, &run.Access.Visibility,
		&run.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, fmt.Errorf("trajectory: run %s: %w", publicID, ErrRunNotFound)
		}
		return nil, 0, fmt.Errorf("trajectory: load run %s: %w", publicID, err)
	}
	if endedAt != nil {
		run.EndedAt = *endedAt
	}
	return &run, runID, nil
}

// loadSpans reads every span of a run in (depth, seq) order — a stable order in
// which parents precede children — as a flat slice for tree assembly.
func (s *Service) loadSpans(ctx context.Context, runID int64) ([]*Span, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT span_id, COALESCE(parent_span_id, ''), depth, seq, category, name,
		       COALESCE(tool, ''), args, output_inline, output_ref_sha256,
		       output_size, output_truncated, start_offset_ms, duration_ms
		FROM spans
		WHERE run_id = $1
		ORDER BY depth, seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("trajectory: load spans: %w", err)
	}
	defer rows.Close()

	var flat []*Span
	for rows.Next() {
		var (
			sp        Span
			args      []byte
			inline    *string
			refSHA    *string
			truncated bool
			size      int64
		)
		if err := rows.Scan(
			&sp.SpanID, &sp.ParentSpanID, &sp.Depth, &sp.Seq, &sp.Category, &sp.Name,
			&sp.Tool, &args, &inline, &refSHA, &size, &truncated,
			&sp.StartOffsetMS, &sp.DurationMS,
		); err != nil {
			return nil, fmt.Errorf("trajectory: scan span: %w", err)
		}
		if len(args) > 0 {
			sp.Args = json.RawMessage(args)
		}
		if inline != nil {
			sp.Inline = *inline
		}
		if refSHA != nil {
			sp.Ref = &OutputRef{SHA256: *refSHA, Size: size, Truncated: truncated}
		}
		flat = append(flat, &sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trajectory: iterate spans: %w", err)
	}
	return flat, nil
}

// attachProducedEdges resolves each write span's produced-artifact public ids
// and hangs them on the matching span, so the produced edge resolves from the
// run direction (SPEC-0004 "Produced-Artifact Link").
func (s *Service) attachProducedEdges(ctx context.Context, runID int64, flat []*Span) error {
	rows, err := s.pool.Query(ctx, `
		SELECT pe.span_id, a.public_id
		FROM produced_edges pe
		JOIN artifacts a ON a.id = pe.artifact_id
		WHERE pe.run_id = $1
		ORDER BY pe.span_id, a.public_id`, runID)
	if err != nil {
		return fmt.Errorf("trajectory: load produced edges: %w", err)
	}
	defer rows.Close()

	bySpan := map[string][]string{}
	for rows.Next() {
		var spanID, producedID string
		if err := rows.Scan(&spanID, &producedID); err != nil {
			return fmt.Errorf("trajectory: scan produced edge: %w", err)
		}
		bySpan[spanID] = append(bySpan[spanID], producedID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("trajectory: iterate produced edges: %w", err)
	}
	for _, sp := range flat {
		if ids, ok := bySpan[sp.SpanID]; ok {
			sp.ProducedArtifactIDs = ids
		}
	}
	return nil
}

// OutputInfo describes a span output opened for streaming download.
type OutputInfo struct {
	SHA256    string
	Size      int64
	Truncated bool
	Inline    bool
}

// OpenSpanOutput opens a span's output for lazy streaming — the deferred fetch
// the viewer performs only when a span is expanded (SPEC-0004 "Large outputs
// MUST be fetched lazily"). Inline outputs stream straight from the row; spilled
// outputs stream from their content-addressed blob. Unknown run/span ids return
// a uniform not-found. The caller must Close the returned reader.
func (s *Service) OpenSpanOutput(ctx context.Context, publicID, spanID string) (io.ReadCloser, OutputInfo, error) {
	var runID int64
	err := s.pool.QueryRow(ctx,
		`SELECT r.id FROM runs r JOIN artifacts a ON a.id = r.artifact_id
		 WHERE a.public_id = $1 AND a.expires_at > now()`, publicID).Scan(&runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, OutputInfo{}, fmt.Errorf("trajectory: run %s: %w", publicID, ErrRunNotFound)
		}
		return nil, OutputInfo{}, fmt.Errorf("trajectory: resolve run %s: %w", publicID, err)
	}

	var (
		inline    *string
		refSHA    *string
		size      int64
		truncated bool
	)
	err = s.pool.QueryRow(ctx, `
		SELECT output_inline, output_ref_sha256, output_size, output_truncated
		FROM spans WHERE run_id = $1 AND span_id = $2`, runID, spanID,
	).Scan(&inline, &refSHA, &size, &truncated)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, OutputInfo{}, fmt.Errorf("trajectory: span %s/%s: %w", publicID, spanID, errs.ErrNotFound)
		}
		return nil, OutputInfo{}, fmt.Errorf("trajectory: load span output %s/%s: %w", publicID, spanID, err)
	}

	if refSHA != nil {
		var storageKey string
		if err := s.pool.QueryRow(ctx,
			`SELECT storage_key FROM blobs WHERE sha256 = $1`, *refSHA,
		).Scan(&storageKey); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, OutputInfo{}, fmt.Errorf("trajectory: output blob %s: %w", *refSHA, errs.ErrNotFound)
			}
			return nil, OutputInfo{}, fmt.Errorf("trajectory: load output blob %s: %w", *refSHA, err)
		}
		rc, err := s.obj.Get(ctx, storageKey)
		if err != nil {
			return nil, OutputInfo{}, fmt.Errorf("trajectory: open output blob %s: %w", *refSHA, err)
		}
		return rc, OutputInfo{SHA256: *refSHA, Size: size, Truncated: truncated}, nil
	}

	text := ""
	if inline != nil {
		text = *inline
	}
	return io.NopCloser(strings.NewReader(text)), OutputInfo{Size: size, Truncated: truncated, Inline: true}, nil
}

// compile-time assurance the produced-edge target is an ordinary artifact type.
var _ artifact.ShareType = artifact.TypeTrajectory
