package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/trajectory"
)

// handleRunStream is the live span SSE endpoint (GET /v1/runs/{id}/stream,
// SPEC-0004 "Live Span Stream Delivery" / ADR-0012 SSE transport). It is a
// replay-then-tail stream: a connecting viewer first receives the spans captured
// so far, then subscribes to live appends, so it shows history AND live arrivals
// with no gap and no duplicate. A reconnecting client resumes after its
// Last-Event-ID with the same guarantee. It is a link-capability read (no auth):
// possession of the unguessable run id grants it, an unknown/expired id is a
// uniform 404 (ADR-0007).
//
// Ordering is what makes it race-safe: it subscribes to the live fan-out BEFORE
// reading the replay snapshot, so an append landing in that window is buffered
// on the subscription rather than lost; the tail then skips any buffered event
// whose stream_seq the replay already covered, so nothing is sent twice. The
// subscription is torn down on client disconnect, on the run closing, or on the
// viewer falling too far behind (it reconnects and resumes) — every exit path
// runs the deferred unsubscribe, so no goroutine or subscription leaks
// (SPEC-0004 "Disconnected SSE client is reaped").
//
// Governing: SPEC-0004 (Live Span Stream Delivery, Concurrency Safety), ADR-0012
// (SSE transport), ADR-0007 (link-capability reads).
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	afterSeq := lastEventID(r)

	// Subscribe FIRST so an append between the replay read and the tail loop is
	// buffered, not dropped. Torn down on every exit path.
	sub := s.traj.Subscribe(id)
	defer sub.Close()

	// Replay snapshot: the run's spans after the resume cursor, plus its current
	// status. Resolve the run here (uniform 404 for unknown/expired) BEFORE any
	// SSE header is written, so an error is a normal JSON envelope.
	spans, status, err := s.traj.SpansAfter(r.Context(), id, afterSeq)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy response buffering so events flush to the browser immediately.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Opening comment establishes the stream (and surfaces an immediately-gone
	// client as a write error).
	if err := writeSSEComment(w, "ok"); err != nil {
		return
	}

	// Replay history in stream order, tracking the highest cursor sent so the
	// tail can de-duplicate against the live fan-out.
	lastSeq := afterSeq
	for _, sp := range spans {
		if err := writeSSESpan(w, sp); err != nil {
			return
		}
		lastSeq = sp.StreamSeq
	}
	if err := writeSSEStatus(w, status); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}
	// A run already closed at connect has no live tail: the viewer has the full
	// history and the closed badge, so end cleanly.
	if status == trajectory.StatusClosed {
		return
	}

	// Tail live appends until the client leaves, the run closes, or the viewer
	// lags out.
	heartbeat := time.NewTicker(s.cfg.StreamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Client disconnected: the deferred unsubscribe reaps this stream.
			return
		case <-sub.Lagged():
			// Fell too far behind the fan-out; end so the client reconnects and
			// resumes from its Last-Event-ID (no span is lost — the rows persist).
			return
		case ev := <-sub.Events():
			switch ev.Type {
			case trajectory.EventSpan:
				if ev.StreamSeq <= lastSeq {
					continue // already covered by the replay snapshot
				}
				if err := writeSSESpan(w, trajectory.StreamSpan{Span: ev.Span, StreamSeq: ev.StreamSeq}); err != nil {
					return
				}
				lastSeq = ev.StreamSeq
			case trajectory.EventStatus:
				if ev.Status == trajectory.StatusClosed {
					_ = writeSSEStatus(w, trajectory.StatusClosed)
					_ = rc.Flush()
					return
				}
				continue
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-heartbeat.C:
			if err := writeSSEComment(w, "heartbeat"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// lastEventID resolves the SSE resume cursor: the standard Last-Event-ID header
// a reconnecting EventSource resends, falling back to a ?last_event_id= query
// param for the initial connect (where a browser cannot set the header). An
// absent or unparseable value means "from the beginning" (0).
func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeSSESpan emits one span as an SSE event, keyed by its stream_seq (the id:
// a reconnecting client resumes after). The span args, output, tool, and name
// are untrusted agent content; encoding/json escapes them so they are inert
// wherever the event is consumed (SPEC-0004 "Active content in a span output").
func writeSSESpan(w io.Writer, sp trajectory.StreamSpan) error {
	payload, err := json.Marshal(toStreamSpanView(sp))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: span\ndata: %s\n\n", sp.StreamSeq, payload)
	return err
}

// writeSSEStatus emits the run's lifecycle status so the viewer's live badge
// reflects open→live→closed (SPEC-0004 "Live-status transitions").
func writeSSEStatus(w io.Writer, status trajectory.Status) error {
	_, err := fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", string(status))
	return err
}

// writeSSEComment emits an SSE comment line (heartbeat / stream-open marker),
// ignored by EventSource but enough to keep the connection warm and to surface a
// vanished client as a write error.
func writeSSEComment(w io.Writer, text string) error {
	_, err := fmt.Fprintf(w, ": %s\n\n", text)
	return err
}

// streamSpanView is one streamed span: the flat span projection plus its
// stream_seq cursor, so a client placing it by parent_span_id/depth/seq can also
// record where to resume. Children are never nested here — the stream delivers
// spans one at a time in ingest order.
type streamSpanView struct {
	spanView
	StreamSeq int64 `json:"stream_seq"`
}

// toStreamSpanView projects a StreamSpan onto its flat wire view (no children).
func toStreamSpanView(sp trajectory.StreamSpan) streamSpanView {
	v := streamSpanView{
		spanView: spanView{
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
		},
		StreamSeq: sp.StreamSeq,
	}
	if sp.Ref != nil {
		v.OutputRef = &outputRefView{SHA256: sp.Ref.SHA256, Size: sp.Ref.Size, Truncated: sp.Ref.Truncated}
	}
	return v
}
