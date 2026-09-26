// The webhook open ingress: the single Public, anonymous-write endpoint in
// Cairn (SPEC-0005 HTTP endpoints table, `ANY /h/{id}`; ADR-0010 "Security of
// an open ingress endpoint"). External senders — services, agents, anything
// pointed at the endpoint's ingress URL — hit this route with no credential
// of any kind; it captures the inbound request into the endpoint's
// seq-ordered ring buffer (webhook.Service.Capture, story #83) and always
// answers with the SAME fixed, benign, inert response, never anything the
// payload steered (SPEC-0005 REQ "Fixed Benign Response").
//
// This is the single most exposed surface in Cairn, so every guard here is
// load-bearing, not routine (SPEC-0005 "Security Requirements ... MANDATORY
// and CRITICAL"):
//
//   - Inert capture — the payload is parsed only enough to store and index
//     (content-type, size); it is NEVER executed, deserialized-to-execute,
//     evaluated, or followed (REQ "No Payload Execution / Inert Capture").
//     This handler does not even attempt to interpret the body's content
//     type; it hands the raw bytes straight to Capture.
//   - Body-size cap BEFORE buffering — http.MaxBytesReader aborts the read
//     the instant the configured cap is exceeded, so an oversize sender is
//     413'd without the full body ever landing in memory or object storage
//     (REQ "Request Body Size Limits").
//   - Per-source-IP AND per-endpoint rate limiting, each its OWN dedicated
//     limiter (never sharing a budget with authenticated traffic) — a flood
//     from one IP or at one endpoint is 429'd and captures nothing (REQ
//     "Rate Limiting").
//   - Header hygiene — delegated to webhook.Capture's sanitizeHeaders
//     (webhook/sanitize.go), which drops Cookie/hop-by-hop headers and masks
//     Authorization's value before a row is ever written, so a captured
//     record is a sanitized projection, never a replayable credential dump
//     (REQ "Header Hygiene & Ephemerality as Containment").
//   - Credential masking — Capture scans the query, headers and body with
//     the ingest scanner and stores only the masked form (SPEC-0017 RD-4).
//     It never refuses a capture over what it finds: a field it cannot store
//     safely is withheld, and this handler's response is unchanged.
//   - No SSRF — Cairn never fetches or follows any URL a payload contains;
//     this handler only ever writes bytes it already has to storage (REQ
//     "Redirect & SSRF Validation").
//   - Write-only — Capture's return value is discarded entirely; a poster
//     gets back a fixed acknowledgment, never the captured stream, another
//     request, or any signal about the endpoint's contents (SPEC-0005
//     "Ingress grants no read").
//   - Unguessable id, uniform 404 — an unknown or expired endpoint id maps
//     to the SAME ErrEndpointNotFound Capture already returns uniformly
//     (REQ "Unguessable ID & No Enumeration"), exactly mirroring every other
//     link-capability read in this adapter.
//   - Opaque internal failures — an unexpected Capture failure (e.g. an
//     object-storage write error) is logged server-side with the request id
//     and answered with the ordinary fixed response, never a stack trace or
//     internal detail (REQ "Ingress error is opaque to the caller").
//
// This route bypasses Pocket ID/OIDC and the session/OAuth auth stack
// entirely — deliberately: it is a machine ingress like /v1 and /mcp, public
// by design (SPEC-0005 "The ingress row is the single Public endpoint in
// Cairn"). It carries the strict /v1-style CSP (never the HTMX/Alpine
// webCSP) because, even though it renders no HTML, it must never loosen the
// hardened baseline on the surface attackers reach first.
//
// Governing: ADR-0010, SPEC-0005 (Security Requirements section).
package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/webhook"
)

// hookIngressPathPrefix is the ingress route's fixed path segment, shared by
// mountHookIngress (registration, below) and hookIngressURL (the address
// hooks.go reports back to the owner, SPEC-0005 "Endpoint exposes both
// addresses") so the two can never drift apart.
const hookIngressPathPrefix = "/h/"

// mountHookIngress registers the open ingress route on the given router
// (already under the strict-CSP group, outside /v1). r.Handle matches ANY
// HTTP method on the pattern (chi Mux.Handle with no method prefix), per
// SPEC-0005 "accepts inbound requests of any method".
func (s *Server) mountHookIngress(r chi.Router) {
	r.Handle(hookIngressPathPrefix+"{id}", http.HandlerFunc(s.handleHookIngress))
}

// hookIngressURL builds the public HTTP ingress address for a webhook
// endpoint — the URL external, unauthenticated senders point at (SPEC-0005
// "a public URL ... e.g. cairn.sh/h/<id>"; ADR-0010). It is a plain path
// join, not a registry URLPrefixer call, because the ingress is a fixed
// top-level route for every webhook endpoint, never type-specific like the
// web/MCP prefixes in api.go's webURL/mcpHandle.
func (s *Server) hookIngressURL(publicID string) string {
	return s.cfg.BaseURL + hookIngressPathPrefix + publicID
}

// hookIngressAck is the fixed, benign body every successful capture returns —
// literal bytes, never anything derived from or reflecting the inbound
// payload (SPEC-0005 REQ "Fixed Benign Response": "The response Cairn
// returns MUST be data it records, never behavior the payload can steer").
const hookIngressAck = "captured\n"

// handleHookIngress is the open, unauthenticated `ANY /h/{id}` capture route.
// See the package doc comment above for the full security rationale; this
// function is deliberately linear so every guard is visible in one place.
func (s *Server) handleHookIngress(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	reqID := middleware.GetReqID(ctx)

	// Per-source-IP, then per-endpoint: two independent dedicated budgets
	// (SPEC-0005 REQ "Rate Limiting": "rate-limited per-endpoint AND
	// per-source-IP"). Neither shares a bucket with the general per-IP
	// limiter (s.limiter, applied to every route including this one) or with
	// each other, so a flood keyed on the sender's IP cannot exhaust the
	// budget legitimate senders at OTHER endpoints depend on, and vice
	// versa. Nothing is captured on a throttled request.
	if s.hookIPLimiter != nil {
		if ok, retry := s.hookIPLimiter.allow(clientIP(r)); !ok {
			s.writeRateLimited(w, r, retry)
			return
		}
	}
	if s.hookEndpointLimiter != nil {
		if ok, retry := s.hookEndpointLimiter.allow(id); !ok {
			s.writeRateLimited(w, r, retry)
			return
		}
	}

	// Hard body-size cap enforced BEFORE buffering (SPEC-0005 REQ "Request
	// Body Size Limits": "rejected with 413 ... before the full body is
	// buffered"). http.MaxBytesReader aborts the underlying read the instant
	// more than hookMaxBodyBytes have been read, so io.ReadAll never
	// allocates past the cap and — because we never reach the Capture call
	// below on this branch — no partial blob is ever persisted. The cap
	// itself is read from webhook.Service.MaxBodyBytes at construction, so
	// this can never silently drift from what Capture would accept anyway.
	r.Body = http.MaxBytesReader(w, r.Body, s.hookMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, r, errs.ErrTooLarge, nil)
			return
		}
		// The sender aborted mid-body (client disconnect, broken chunked
		// stream, etc). There is no coherent request to capture and no live
		// connection to answer on some transports either way; log with the
		// request id and stop rather than guess at a response.
		s.log.InfoContext(ctx, "httpapi: hook ingress body read aborted", "id", id, "request_id", reqID, "error", err)
		return
	}

	// Inert capture: Cairn parses nothing beyond what it stores and indexes.
	// The body goes to Capture as opaque bytes; the response status is
	// ALWAYS the fixed default, never derived from or steerable by the
	// payload's headers, body, or method (SPEC-0005 REQ "No Payload
	// Execution / Inert Capture", "Fixed Benign Response"). Capture's return
	// value — the captured Request — is discarded entirely: a poster can
	// never read back what it just sent (SPEC-0005 "Ingress grants no
	// read").
	status := webhook.DefaultResponseStatus
	_, err = s.hook.Capture(ctx, id, webhook.CaptureInput{
		Method:      r.Method,
		Path:        r.URL.Path,
		Query:       r.URL.RawQuery,
		Headers:     r.Header,
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
		Status:      status,
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeNotFound, errs.CodePayloadTooLarge, errs.CodeValidation:
			// Unknown/expired endpoint (uniform 404, SPEC-0005 "Unguessable ID
			// & No Enumeration"), an over-cap body Capture itself rejected
			// (413, belt-and-suspenders behind the transport cap above), or a
			// malformed capture (400) all get their real, distinguishable
			// status — none of these leak anything the caller doesn't already
			// know from its own request.
			s.writeError(w, r, err, nil)
			return
		default:
			// Anything else — an object-storage write failure, a database
			// error — is an internal failure the anonymous caller must never
			// see (SPEC-0005 REQ "Error Handling Standards": "Scenario:
			// Ingress error is opaque to the caller"). Log it with the
			// request id server-side and fall through to the SAME fixed
			// response a successful capture would have gotten.
			s.log.ErrorContext(ctx, "httpapi: hook capture failed", "id", id, "request_id", reqID, "error", err)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(hookIngressAck))
	}
}
