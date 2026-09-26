package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/trajectory"
)

// runMux contributes the trajectory share type's /v1/runs* ingest, lifecycle,
// and lazy-output surface through the ADR-0002 RouteMounter capability. The
// trajectory service is a runtime dependency (a live Postgres pool + object
// store), so the adapter captures it at construction and mounts its routes
// through this seam rather than the core router hard-coding trajectory-specific
// paths — exactly the seam an out-of-tree share type would use to arrive by
// registration alone. Writes require authentication and pass the CSRF seam (a
// no-op for token callers, the guard the future cookie-session surface plugs
// into); reads follow the run artifact's ADR-0007 link capability, so a valid
// id reads and an unknown/expired id is a uniform 404.
//
// Governing: ADR-0002 (RouteMounter capability), ADR-0012 (chi sub-routers,
// error envelope), SPEC-0004 (Trajectory Share — Run Ingestion), ADR-0007
// (link-capability reads).
type runMux struct{ s *Server }

// runMux satisfies the RouteMounter capability the registry seam iterates.
var _ sharetype.RouteMounter = runMux{}

// MountRoutes registers the trajectory /v1/runs* endpoints on the /v1 router.
func (m runMux) MountRoutes(r chi.Router) {
	s := m.s
	r.With(s.requireAuth, s.enforceCSRF).Post("/runs", s.handleCreateRun)
	r.With(s.requireAuth, s.enforceCSRF).Post("/runs/{id}/spans", s.handleAppendSpans)
	r.With(s.requireAuth, s.enforceCSRF).Post("/runs/{id}/close", s.handleCloseRun)
	r.Get("/runs/{id}", s.handleGetRun)
	r.Get("/runs/{id}/spans/{spanID}/output", s.handleGetSpanOutput)
	r.Get("/runs/{id}/stream", s.handleRunStream)
}

// runRequest is the POST /v1/runs body. Mode selects the ingest shape: "batch"
// (default) ingests a complete run created closed; "open" starts a live run and
// returns its shareable link immediately, optionally seeding it with initial
// spans. Provenance channel, owner, capture time, and expiry are all
// server-derived from the authenticated principal — never a client claim
// (SPEC-0002 "Channel is server-derived"); only on_behalf_of records the human
// an agent is acting for.
type runRequest struct {
	Mode       string        `json:"mode,omitempty"`
	Title      string        `json:"title,omitempty"`
	Prompt     string        `json:"prompt,omitempty"`
	Model      string        `json:"model,omitempty"`
	TokenCount int64         `json:"token_count,omitempty"`
	StartedAt  time.Time     `json:"started_at,omitempty"`
	OnBehalfOf string        `json:"on_behalf_of,omitempty"`
	Spans      []spanRequest `json:"spans,omitempty"`
}

// appendSpansRequest is the POST /v1/runs/{id}/spans body: one or more spans to
// append to an open run. Re-posting spans already present is an idempotent
// no-op (SPEC-0004 "appends are additive only").
type appendSpansRequest struct {
	Spans []spanRequest `json:"spans"`
}

// spanRequest is one ingested span. depth and seq are NOT client-supplied — the
// service derives them from the parent chain and sibling order. output carries
// the raw bytes (base64 in JSON); the service inlines a small one or spills a
// large one to a content-addressed blob.
type spanRequest struct {
	SpanID             string          `json:"span_id"`
	ParentSpanID       string          `json:"parent_span_id,omitempty"`
	Category           string          `json:"category"`
	Name               string          `json:"name,omitempty"`
	Tool               string          `json:"tool,omitempty"`
	Args               json.RawMessage `json:"args,omitempty"`
	Output             []byte          `json:"output,omitempty"`
	OutputTruncated    bool            `json:"output_truncated,omitempty"`
	StartOffsetMS      int             `json:"start_offset_ms"`
	DurationMS         int             `json:"duration_ms"`
	ProducedArtifactID string          `json:"produced_artifact_id,omitempty"`
}

// runResponse is the JSON view of a run: its artifact envelope, shareable URLs,
// derived stats, and its ordered span tree.
type runResponse struct {
	ID         string         `json:"id"`
	URL        string         `json:"url"`
	MCP        string         `json:"mcp"`
	Status     string         `json:"status"`
	Title      string         `json:"title,omitempty"`
	Prompt     string         `json:"prompt,omitempty"`
	Model      string         `json:"model,omitempty"`
	TokenCount int64          `json:"token_count"`
	Provenance provenanceView `json:"provenance"`
	StartedAt  time.Time      `json:"started_at"`
	EndedAt    *time.Time     `json:"ended_at,omitempty"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Stats      statsView      `json:"stats"`
	Spans      []spanView     `json:"spans"`
}

type statsView struct {
	WallTimeMS       int64            `json:"wall_time_ms"`
	SpanCount        int              `json:"span_count"`
	ToolCallCount    int              `json:"tool_call_count"`
	TokenCount       int64            `json:"token_count"`
	TimeByCategoryMS map[string]int64 `json:"time_by_category_ms"`
}

type spanView struct {
	SpanID              string          `json:"span_id"`
	ParentSpanID        string          `json:"parent_span_id,omitempty"`
	Depth               int             `json:"depth"`
	Seq                 int             `json:"seq"`
	Category            string          `json:"category"`
	Name                string          `json:"name,omitempty"`
	Tool                string          `json:"tool,omitempty"`
	Args                json.RawMessage `json:"args,omitempty"`
	Output              string          `json:"output,omitempty"`
	OutputRef           *outputRefView  `json:"output_ref,omitempty"`
	StartOffsetMS       int             `json:"start_offset_ms"`
	DurationMS          int             `json:"duration_ms"`
	ProducedArtifactIDs []string        `json:"produced_artifact_ids,omitempty"`
	Children            []spanView      `json:"children,omitempty"`
}

// outputRefView describes a span's large output stored as a content-addressed
// blob, fetched lazily via GET /v1/runs/{id}/spans/{span_id}/output.
type outputRefView struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
}

// handleCreateRun ingests a run: a complete batch run (created closed) or an
// open live run whose id and shareable URL are returned immediately (SPEC-0004
// "Run Ingestion — Batch and Incremental", "Open returns a shareable link
// immediately"). Both shapes converge to the identical stored run.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	var req runRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, r, err, nil)
		return
	}

	now := s.now()
	started := req.StartedAt
	if started.IsZero() {
		started = now
	}
	in := trajectory.RunInput{
		Title:      req.Title,
		Prompt:     req.Prompt,
		Model:      req.Model,
		TokenCount: req.TokenCount,
		StartedAt:  started,
		Provenance: artifact.Provenance{
			ActorID:    p.ActorID,
			OnBehalfOf: req.OnBehalfOf,
			// The run already names its model; mirror it onto provenance so a
			// trajectory reads the same as every other artifact.
			Model:      req.Model,
			Channel:    p.Channel,
			CapturedAt: now,
		},
		Access:    artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink},
		ExpiresAt: now.Add(s.cfg.DefaultTTL),
		Spans:     toSpanInputs(req.Spans),
	}

	var (
		run *trajectory.Run
		err error
	)
	switch req.Mode {
	case "", "batch":
		run, err = s.traj.CreateBatchRun(r.Context(), in)
	case "open":
		run, err = s.traj.OpenRun(r.Context(), in)
	default:
		s.writeError(w, r, errs.Validationf("mode must be \"batch\" or \"open\""), nil)
		return
	}
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toRunResponse(run))
}

// handleAppendSpans appends one or more spans to an open run (incremental
// ingest). Appending to a closed run is a conflict; a non-owner is forbidden; a
// span naming an unknown parent is a validation failure that persists none of
// the payload; and re-posting already-present spans is an idempotent no-op
// (SPEC-0004 "Malformed tree rejected atomically", "Append to a closed run
// refused"). It returns the run's current state so batch and incremental read
// back identically.
func (s *Server) handleAppendSpans(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	var req appendSpansRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	if _, err := s.traj.AppendSpans(r.Context(), id, p.ActorID, toSpanInputs(req.Spans)); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	run, err := s.traj.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toRunResponse(run))
}

// handleCloseRun closes an open run, stamping ended_at from the span tree so it
// matches a batch run and freezing it against further spans. Owner-only; a
// closed run stays closed (SPEC-0004 "Run Model and Lifecycle").
func (s *Server) handleCloseRun(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	run, err := s.traj.CloseRun(r.Context(), id, p.ActorID)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toRunResponse(run))
}

// handleGetRun fetches a run's metadata, ordered span tree, and derived stats.
// Link-capability read: a valid id reads, an unknown/expired id is a uniform
// 404 (ADR-0007, SPEC-0004 "Derived Run Statistics").
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.traj.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toRunResponse(run))
}

// handleGetSpanOutput lazily streams a span's output — the deferred fetch the
// viewer performs only when a span is expanded (SPEC-0004 "Large outputs MUST
// be fetched lazily"). It streams inline or content-addressed output alike with
// a non-executable download disposition, and returns a uniform 404 for an
// unknown run or span. Link-capability read.
func (s *Server) handleGetSpanOutput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spanID := chi.URLParam(r, "spanID")
	rc, info, err := s.traj.OpenSpanOutput(r.Context(), id, spanID)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "span_id": spanID})
		return
	}
	defer rc.Close()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(spanID+".output"))
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.SHA256 != "" {
		h.Set("X-Cairn-Checksum", info.SHA256)
		h.Set("ETag", strconv.Quote(info.SHA256))
	}
	if info.Truncated {
		h.Set("X-Cairn-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		s.log.WarnContext(r.Context(), "httpapi: span output stream aborted", "error", err)
	}
}

// decodeJSONBody caps the request body at MaxRunRequestBytes and decodes it as
// JSON. An oversize payload is 413 before the whole body is buffered; malformed
// JSON is validation_failed (SPEC-0004 endpoint security — request-size caps).
// Shared by the trajectory run/append and webhook endpoint-create bodies
// (hooks.go), which are both small, server-derived-envelope JSON payloads.
func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRunRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errs.ErrTooLarge
		}
		return errs.Validationf("request body is not valid JSON")
	}
	return nil
}

// toSpanInputs converts the wire spans to the service's SpanInput values.
func toSpanInputs(reqs []spanRequest) []trajectory.SpanInput {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]trajectory.SpanInput, 0, len(reqs))
	for _, sp := range reqs {
		out = append(out, trajectory.SpanInput{
			SpanID:             sp.SpanID,
			ParentSpanID:       sp.ParentSpanID,
			Category:           trajectory.Category(sp.Category),
			Name:               sp.Name,
			Tool:               sp.Tool,
			Args:               sp.Args,
			Output:             sp.Output,
			OutputTruncated:    sp.OutputTruncated,
			StartOffsetMS:      sp.StartOffsetMS,
			DurationMS:         sp.DurationMS,
			ProducedArtifactID: sp.ProducedArtifactID,
		})
	}
	return out
}

// toRunResponse projects a service Run onto its JSON view, building the run's
// short URL and MCP handle from the trajectory type's registry URL prefixes
// (never a switch on the type here — ADR-0002/ADR-0005).
func (s *Server) toRunResponse(run *trajectory.Run) runResponse {
	pref := s.reg.URLPrefixFor(artifact.TypeTrajectory)
	resp := runResponse{
		ID:         run.PublicID,
		URL:        s.cfg.BaseURL + prefixedPath(pref.Web, run.PublicID),
		MCP:        "mcp://cairn" + prefixedPath(pref.MCP, run.PublicID),
		Status:     string(run.Status),
		Title:      run.Title,
		Prompt:     run.Prompt,
		Model:      run.Model,
		TokenCount: run.TokenCount,
		Provenance: provenanceView{
			Actor:      run.Provenance.ActorID,
			OnBehalfOf: run.Provenance.OnBehalfOf,
			Channel:    run.Provenance.Channel,
			CapturedAt: run.Provenance.CapturedAt,
		},
		StartedAt: run.StartedAt,
		ExpiresAt: run.ExpiresAt,
		Stats: statsView{
			WallTimeMS:       run.Stats.WallTimeMS,
			SpanCount:        run.Stats.SpanCount,
			ToolCallCount:    run.Stats.ToolCallCount,
			TokenCount:       run.Stats.TokenCount,
			TimeByCategoryMS: categoryMap(run.Stats.TimeByCategoryMS),
		},
		Spans: toSpanViews(run.Spans),
	}
	if !run.EndedAt.IsZero() {
		ended := run.EndedAt
		resp.EndedAt = &ended
	}
	return resp
}

func categoryMap(in map[trajectory.Category]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

// toSpanViews recursively projects the ordered span tree onto its JSON view.
func toSpanViews(spans []*trajectory.Span) []spanView {
	if len(spans) == 0 {
		return nil
	}
	out := make([]spanView, 0, len(spans))
	for _, sp := range spans {
		v := spanView{
			SpanID:              sp.SpanID,
			ParentSpanID:        sp.ParentSpanID,
			Depth:               sp.Depth,
			Seq:                 sp.Seq,
			Category:            string(sp.Category),
			Name:                sp.Name,
			Tool:                sp.Tool,
			Args:                sp.Args,
			Output:              sp.Inline,
			StartOffsetMS:       sp.StartOffsetMS,
			DurationMS:          sp.DurationMS,
			ProducedArtifactIDs: sp.ProducedArtifactIDs,
			Children:            toSpanViews(sp.Children),
		}
		if sp.Ref != nil {
			v.OutputRef = &outputRefView{SHA256: sp.Ref.SHA256, Size: sp.Ref.Size, Truncated: sp.Ref.Truncated}
		}
		out = append(out, v)
	}
	return out
}
