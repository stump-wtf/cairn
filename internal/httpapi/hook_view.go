package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"sort"
	"strconv"
	"strings"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/code"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/webhook"
)

// The webhook inspector viewer (SPEC-0005 REQ "Inspector Viewer", ADR-0011,
// issue #86): a live-updating list of captured requests plus a per-request
// inspect view (method/path/status/headers/highlighted JSON body) and a
// status-mix summary, all computed from the webhook service's PostgreSQL-
// resident request rows — never from a body (SPEC-0005 "Metadata queryable
// without reading the body"). Like the trajectory and bundle viewers, a
// webhook endpoint is bodyless (its content is a live capture log, not a
// content-addressed blob), so buildShellView resolves this path through the
// registry StreamViewer capability (StreamViewerFor) rather than a switch on
// the type key, and this file delegates straight to the webhook service the
// web server already holds (s.hook) — exactly as bundle_view.go delegates to
// the store and trajectory_view.go to the trajectory service.
//
// The whole page is server-rendered so the request list, the status mix, and
// every captured request's method/path/headers/body are present and readable
// with no JavaScript (SPEC-0005 REQ "Inspector Viewer": progressive
// enhancement); hook.js only layers on the live SSE tail (mirroring the
// trajectory viewer's live span append) and the reaction picker (#66 pattern,
// reusing the SAME "reaction-cluster" template trajectory.html defines — see
// the SpanID field note on hookRequestRow.React below).
//
// SPEC-0005 REQ "v1 Non-Goals Are Absent" forbids a "first-request empty-state
// AFFORDANCE" — a richer onboarding feature (e.g. an interactive "send a test
// request" walkthrough) — in v1, while issue #86's checklist explicitly asks
// for "Empty-state for a bin awaiting its first request" and a browser test
// asserting "empty-state renders". This implementation reconciles the two by
// rendering only a minimal, non-interactive empty state: a single sentence
// plus the already-elsewhere-shown ingress URL as plain text (no new
// affordance, no wizard, no copy-to-clipboard button beyond the one the
// header's own URL control already offers) — graceful zero-state handling of
// a list that would otherwise render as a blank box, not the excluded
// "try next" feature.
//
// Governing: ADR-0011, ADR-0002 (registry resolution; no switch on type),
// ADR-0010 (live webhook endpoints), ADR-0007 (link-capability read, uniform
// 404), SPEC-0005 (Webhook Inspector), SPEC-0006 (registry-gated anchors).

// hookView is the fully server-computed view model the webhook body slot
// renders. Every field is derived from the webhook service's request rows +
// the annotation service, so the page carries no client data layer
// (ADR-0011).
type hookView struct {
	ID         string
	IngressURL string
	RequestCap int
	// Live is always true when this view renders: an endpoint past its TTL
	// never reaches buildHookView at all — renderShellFor's link-capability
	// read already resolves an expired id to the uniform 404 upstream
	// (SPEC-0005 "Expired endpoint stops capturing") — so there is no
	// "closed" state to represent, unlike a trajectory run.
	Live       bool
	Empty      bool
	TotalCount int
	StatusMix  []hookStatusSeg
	Requests   []hookRequestRow
	// React is the whole-endpoint reaction cluster (the `artifact` anchor,
	// the one webhook anchor the SPEC-0006 capability matrix also grants
	// alongside webhook_request — "webhook reactions: artifact,
	// webhook_request").
	React reactionCluster
}

// hookStatusSeg is one bucket of the status-mix summary (SPEC-0005 "a status
// mix summarizing the response-status distribution across the buffer"). Class
// is the status family ("2xx"/"4xx"/…) shown as visible TEXT alongside the
// count, never conveyed by color alone (SPEC-0005 Accessibility REQ "Status
// is not color-only").
type hookStatusSeg struct {
	Class string
	Count int
	Pct   string
}

// hookHeaderLine is one captured, already-sanitized request header, rendered
// name/value pair (SPEC-0005 "Header Hygiene": the row is a sanitized
// projection by the time it reaches this viewer — sanitizeHeaders already ran
// at capture time, internal/webhook/sanitize.go).
type hookHeaderLine struct {
	Name  string
	Value string
}

// hookRequestRow is one captured request: the inspect-view facts SPEC-0005
// REQ "Inspector Viewer" lists (method, path, the fixed response status,
// headers, and the body with render-time JSON syntax highlighting), plus its
// reaction cluster.
type hookRequestRow struct {
	Seq         string // decimal seq, also the webhook_request anchor's request_id
	Method      string
	Path        string
	Query       string
	Status      int
	StatusClass string
	ReceivedAt  string
	ContentType string
	BodySize    string
	Headers     []hookHeaderLine
	// HasBody is false for a captured request with an empty body (BodySize ==
	// 0): neither BodyHTML nor BodyLazy applies. Exactly one of BodyHTML
	// (already rendered — highlighted JSON or escaped plain text, inline
	// bodies only) or BodyLazy (a spilled/oversized body, fetched on expand
	// per SPEC-0005 "Bodies MUST be fetched lazily when a request is
	// expanded") is set when HasBody is true.
	HasBody   bool
	BodyHTML  template.HTML
	BodyLazy  bool
	BodyURL   string
	Truncated bool
	// React reuses the SAME reactionCluster/"reaction-cluster" template
	// trajectory_view.go/trajectory.html define (#66 click-time render
	// pattern): SpanID here carries this request's seq (the stable
	// webhook_request identity, SPEC-0005 "a stable request id — the
	// webhook_request anchor target"), not a trajectory span id — the field
	// is a generic per-cluster item-id slot the template only ever emits as
	// `data-span-id`, and hook.js reads that same attribute to build a
	// `{"request_id": "<seq>"}` anchor_ref rather than trajectory.js's
	// `{"span_id": …}`, matching the webhookRequestLocator schema
	// (internal/sharetype/locator.go).
	React reactionCluster
}

// buildHookView projects a webhook endpoint into the viewer model: its
// ingress URL and ring-buffer cap, the currently-retained request rows
// (newest first, SPEC-0005 "live request list (newest prepended)"), the
// status mix, and the reaction tallies keyed by anchor (whole-endpoint +
// per-request). A resolution or list failure returns nil so the shell falls
// back to the generic-file card — the total-resolution floor is never
// bypassed (ADR-0002), mirroring buildBundleView's nil-on-failure contract.
func (s *Server) buildHookView(ctx context.Context, a *artifact.Artifact) *hookView {
	ep, err := s.hook.GetEndpoint(ctx, a.PublicID)
	if err != nil {
		s.log.WarnContext(ctx, "web: get hook endpoint failed", "id", a.PublicID, "error", err)
		return nil
	}
	// Render up to the read API's own bounded page ceiling (MaxListLimit),
	// wider than its JSON-API page default (DefaultListLimit), so a typical
	// live-viewing session shows its whole retained buffer on first load; the
	// live tail (hook.js) inserts by seq order regardless, so correctness
	// never depends on this number.
	page, err := s.hook.ListRequests(ctx, a.PublicID, 0, webhook.MaxListLimit)
	if err != nil {
		s.log.WarnContext(ctx, "web: list hook requests failed", "id", a.PublicID, "error", err)
		return nil
	}

	tallies := s.reactionIndex(ctx, a.PublicID)
	vm := &hookView{
		ID:         a.PublicID,
		IngressURL: s.hookIngressURL(a.PublicID),
		RequestCap: ep.RequestCap,
		Live:       true,
		Empty:      len(page.Requests) == 0,
		TotalCount: len(page.Requests),
		React: reactionCluster{
			AnchorType: string(sharetype.AnchorArtifact),
			Label:      "Reactions on this endpoint",
			Pills:      tallies[string(sharetype.AnchorArtifact)+"|{}"],
		},
	}

	mix := map[string]int{}
	for _, req := range page.Requests {
		vm.Requests = append(vm.Requests, s.toHookRequestRow(a.PublicID, req, tallies))
		mix[statusClass(req.Status)]++
	}
	vm.StatusMix = buildStatusMix(mix, len(page.Requests))
	return vm
}

// toHookRequestRow projects one captured request onto its inspector row,
// rendering an inline body's highlighted (JSON) or escaped (everything else)
// fragment now — a spilled body stays a lazy reference the viewer fetches on
// expand (SPEC-0005 "Bodies MUST be fetched lazily when a request is
// expanded").
func (s *Server) toHookRequestRow(publicID string, req webhook.Request, tallies map[string][]reactionPill) hookRequestRow {
	seqStr := strconv.FormatInt(req.Seq, 10)
	row := hookRequestRow{
		Seq:         seqStr,
		Method:      req.Method,
		Path:        firstNonEmpty(req.Path, "/"),
		Query:       req.Query,
		Status:      req.Status,
		StatusClass: statusClass(req.Status),
		ReceivedAt:  humanizeSince(req.ReceivedAt),
		ContentType: req.ContentType,
		BodySize:    humanizeBytes(req.BodySize),
	}
	for name, vals := range req.Headers {
		for _, v := range vals {
			row.Headers = append(row.Headers, hookHeaderLine{Name: name, Value: v})
		}
	}
	sort.Slice(row.Headers, func(i, j int) bool {
		if row.Headers[i].Name != row.Headers[j].Name {
			return row.Headers[i].Name < row.Headers[j].Name
		}
		return row.Headers[i].Value < row.Headers[j].Value
	})

	switch {
	case req.BodySize == 0:
		// No body captured; HasBody stays false.
	case req.Inline != nil:
		row.HasBody = true
		row.BodyHTML = renderHookBody(req.Inline, req.ContentType)
	case req.Ref != nil:
		row.HasBody = true
		row.BodyLazy = true
		row.Truncated = req.Ref.Truncated
		row.BodyURL = "/v1/hooks/" + publicID + "/requests/" + seqStr + "/body"
	}

	key := webhookRequestAnchorKey(req.Seq)
	row.React = reactionCluster{
		AnchorType: string(sharetype.AnchorWebhookRequest),
		SpanID:     seqStr,
		Label:      "Reactions on " + req.Method + " " + row.Path,
		Pills:      tallies[string(sharetype.AnchorWebhookRequest)+"|"+key],
	}
	return row
}

// webhookRequestAnchorKey renders the canonical anchor_key for a captured
// request's webhook_request anchor — {"request_id":"<seq>"} — matching
// annotation.CanonicalRef's single-field, sorted-key, no-whitespace form
// (encoding/json already emits that for a struct with one field), so the
// server-rendered tally key matches the stored one without re-deriving it
// (mirrors trajectory_view.go's spanAnchorKey). The seq itself is the
// request's stable identity (SPEC-0005 "a stable request id — the
// webhook_request anchor target", internal/db/migrations/0010_webhook.sql),
// stringified to fit the locator's string request_id field
// (sharetype.webhookRequestLocator).
func webhookRequestAnchorKey(seq int64) string {
	b, _ := json.Marshal(struct {
		RequestID string `json:"request_id"`
	}{strconv.FormatInt(seq, 10)})
	return string(b)
}

// statusClass buckets an HTTP status into its family ("2xx", "4xx", …) for
// the status-mix summary and the per-row status badge — a text cue standing
// alongside (never replacing) any color the badge also carries (SPEC-0005
// Accessibility REQ "Status is not color-only").
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "?"
	}
	return strconv.Itoa(status/100) + "xx"
}

// statusClassOrder is the fixed, ascending display order for the status-mix
// legend, so the same buffer always renders its segments in the same order
// regardless of map iteration (SPEC-0005 "Status mix reflects the buffer").
var statusClassOrder = []string{"1xx", "2xx", "3xx", "4xx", "5xx"}

// buildStatusMix turns a family->count tally into the ordered, percentaged
// segments the panel's status-mix bar + legend render, skipping any family
// absent from the buffer.
func buildStatusMix(counts map[string]int, total int) []hookStatusSeg {
	var out []hookStatusSeg
	for _, class := range statusClassOrder {
		n := counts[class]
		if n == 0 {
			continue
		}
		out = append(out, hookStatusSeg{Class: class, Count: n, Pct: formatPct(ratioPct(n, int64(total)))})
	}
	return out
}

// renderHookBody renders a captured request's inline body as inert, escaped
// text (SPEC-0005 Security REQ "No Payload Execution / Inert Capture": "The
// inspector MUST render payloads as inert escaped text"): JSON bodies get
// render-time syntax highlighting by reusing the code viewer's chroma
// pipeline (internal/code.Render, the SAME pure-Go, HTML-escaping-per-token
// highlighter the code share type's BodyViewer uses, SPEC-0005 "reuse the
// code viewer's chroma"); every other content type is escaped plain text.
// Both paths are equally inert — chroma HTML-escapes every token's text, so
// neither highlighting nor the plain-text fallback can execute active
// content in Cairn's origin, matching codeShareType.RenderBody's guarantee.
func renderHookBody(body []byte, contentType string) template.HTML {
	if looksLikeJSON(contentType, body) {
		if r, err := code.Render(body, "application/json", "", "json"); err == nil {
			var buf strings.Builder
			for i, line := range r.Lines {
				if i > 0 {
					buf.WriteByte('\n')
				}
				buf.WriteString(string(line.HTML))
			}
			return template.HTML(buf.String()) //nolint:gosec // chroma HTML-escapes every token value (internal/code/render.go)
		}
	}
	return template.HTML(template.HTMLEscapeString(string(body))) //nolint:gosec // explicit escape of untrusted captured bytes
}

// looksLikeJSON decides whether a captured body should render through the
// JSON highlighter: an explicit `*/json*` content type is trusted outright;
// otherwise a body that both looks like (starts with `{`/`[`) and validates
// as JSON is highlighted, so a coincidental leading brace in an
// unrelated-content-type body never mis-renders (SPEC-0005 "Captured
// payloads MUST be displayed as inert escaped text" — highlighting is a
// render-time enhancement of that escaped text, never a trust decision).
func looksLikeJSON(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return json.Valid(trimmed)
}
