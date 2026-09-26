// The webhook live SSE stream (`GET /v1/hooks/{id}/stream`, SPEC-0005 REQ
// "One Stream, Two Transports (Live Fan-out)", ADR-0010, ADR-0012 SSE
// transport). It is the browser-facing half of the identical seq-ordered
// capture log the mcp://cairn/hook/<id> MCP resource (mcp.go) reads over the
// other transport — both are thin adapters over webhook.Service's hub and
// RequestsAfter, exactly mirroring internal/httpapi/stream.go's split for the
// trajectory share type.
//
// Governing: SPEC-0005 (Live Fan-out, Concurrency Safety), ADR-0010, ADR-0012
// (SSE transport), ADR-0007 (link-capability reads).
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/webhook"
)

// handleHookStream is the live captured-request SSE endpoint. It is a
// replay-then-tail stream — a connecting inspector first receives the
// requests currently retained in the ring buffer, then subscribes to live
// captures, so it shows history AND live arrivals with no gap and no
// duplicate (SPEC-0005 "Late joiner sees history then tail"). A reconnecting
// client resumes after its Last-Event-ID (the request's own `seq`, already
// the endpoint's stable per-request identity) with the same guarantee
// (SPEC-0005 "SSE reconnection MUST resume from Last-Event-ID").
//
// Ordering is what makes it race-safe: it subscribes to the live fan-out
// BEFORE reading the replay snapshot, so a capture landing in that window is
// buffered on the subscription rather than lost; the tail then skips any
// buffered event whose seq the replay already covered, so nothing is sent
// twice. The subscription is torn down on client disconnect or when the
// viewer falls too far behind (it reconnects and resumes) — every exit path
// runs the deferred unsubscribe, so no goroutine or subscription leaks
// (SPEC-0005 "Disconnected inspector is reaped"). It is a link-capability
// read (no auth): possession of the unguessable endpoint id grants it, an
// unknown/expired id is a uniform 404 (ADR-0007).
func (s *Server) handleHookStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	afterSeq := lastEventID(r)

	// Subscribe FIRST so a capture between the replay read and the tail loop
	// is buffered, not dropped. Torn down on every exit path.
	sub := s.hook.Subscribe(id)
	defer sub.Close()

	// Replay snapshot: the endpoint's currently-retained requests after the
	// resume cursor. Resolve the endpoint here (uniform 404 for unknown/
	// expired) BEFORE any SSE header is written, so an error is a normal JSON
	// envelope.
	reqs, err := s.hook.RequestsAfter(r.Context(), id, afterSeq)
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

	// Replay history in seq order, tracking the highest cursor sent so the
	// tail can de-duplicate against the live fan-out.
	lastSeq := afterSeq
	for _, req := range reqs {
		if err := writeSSERequest(w, req); err != nil {
			return
		}
		lastSeq = req.Seq
	}
	if err := rc.Flush(); err != nil {
		return
	}

	// Tail live captures until the client leaves or the viewer lags out. A
	// webhook endpoint has no open/closed lifecycle transition to signal
	// (unlike a trajectory run) — it stays live until its artifact TTL
	// expires, at which point a reconnecting client simply gets the uniform
	// 404 above (SPEC-0005 "Expired endpoint stops capturing").
	heartbeat := time.NewTicker(s.cfg.StreamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Client disconnected: the deferred unsubscribe reaps this stream.
			return
		case <-sub.Lagged():
			// Fell too far behind the fan-out; end so the client reconnects
			// and resumes from its Last-Event-ID (no capture is lost — the
			// rows persist until evicted by the ring buffer, same as any
			// other reader).
			return
		case ev := <-sub.Events():
			if ev.Type != webhook.EventRequest || ev.Seq <= lastSeq {
				continue // already covered by the replay snapshot
			}
			if err := writeSSERequest(w, *ev.Request); err != nil {
				return
			}
			lastSeq = ev.Seq
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

// writeSSERequest emits one captured request as an SSE event, keyed by its
// seq (the id: a reconnecting client resumes after). It projects onto the
// same requestView the REST read endpoints render (ADR-0003 parity);
// encoding/json escapes every field so an attacker-controlled method, path,
// header, or inline body is inert wherever the event is consumed (SPEC-0005
// "Payload is never executed").
func writeSSERequest(w io.Writer, req webhook.Request) error {
	payload, err := json.Marshal(toRequestView(req))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: request\ndata: %s\n\n", req.Seq, payload)
	return err
}
