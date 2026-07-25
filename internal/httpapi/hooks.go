// hookMux contributes the webhook share type's /v1/hooks* management,
// metadata, and captured-request read surface through the ADR-0002
// RouteMounter capability, exactly mirroring runMux (runs.go) for the other
// live share type. The webhook service is a runtime dependency (a live
// Postgres pool + object store), so the adapter captures it at construction
// and mounts its routes through this seam rather than the core router
// hard-coding webhook-specific paths.
//
// This is the model + management story (issue #83): creation requires
// authentication and passes the CSRF seam; every read follows the endpoint's
// ADR-0007 link capability, so a valid id reads and an unknown/expired id is
// a uniform 404 with no owner check on the read path (SPEC-0005 "Read
// endpoints MUST enforce the ADR-0007 link-capability policy"). The public,
// anonymous-write ingress that actually fills the buffer (`ANY /h/{id}`,
// SPEC-0005) is deliberately NOT mounted on this router group — it carries
// its own security posture entirely (no auth, its own rate limits) and lives
// in hook_ingress.go / api.go's separate top-level route group (issue #84).
//
// Governing: ADR-0002 (RouteMounter capability), ADR-0010 (Live Webhook
// Endpoints), SPEC-0005 (Webhook Inspector — HTTP endpoints table), ADR-0007
// (link-capability reads).
package httpapi

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/webhook"
)

type hookMux struct{ s *Server }

// hookMux satisfies the RouteMounter capability the registry seam iterates.
var _ sharetype.RouteMounter = hookMux{}

// MountRoutes registers the webhook /v1/hooks* endpoints on the /v1 router.
func (m hookMux) MountRoutes(r chi.Router) {
	s := m.s
	r.With(s.requireAuth, s.enforceCSRF).Post("/hooks", s.handleCreateHook)
	r.Get("/hooks/{id}", s.handleGetHook)
	r.Get("/hooks/{id}/requests", s.handleListHookRequests)
	r.Get("/hooks/{id}/requests/{seq}", s.handleGetHookRequest)
	r.Get("/hooks/{id}/requests/{seq}/body", s.handleGetHookRequestBody)
	r.Get("/hooks/{id}/stream", s.handleHookStream)
}

// createHookRequest is the POST /v1/hooks body. Provenance channel, owner,
// capture time, and expiry are all server-derived from the authenticated
// principal — never a client claim (SPEC-0002 "Channel is server-derived");
// only on_behalf_of records the human an agent is acting for. RequestCap lets
// an owner shrink or grow the ring buffer; zero takes the SPEC-0005 default
// (500).
type createHookRequest struct {
	Title      string `json:"title,omitempty"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	RequestCap int    `json:"request_cap,omitempty"`
}

// hookResponse is the JSON view of a webhook endpoint: its artifact envelope,
// the human URL, the public HTTP ingress URL, the mcp://cairn/hook/<id>
// handle, and the ring-buffer cap (SPEC-0005 "Endpoint exposes both
// addresses": "the response MUST include the human cairn.sh/<id> URL, the
// HTTP ingress URL, and the mcp://cairn/hook/<id> handle for the same
// endpoint"). IngressURL is the `ANY /h/{id}` route (hook_ingress.go)
// external, unauthenticated senders point at — a DIFFERENT address than URL
// (the human web-shell page for the same endpoint).
type hookResponse struct {
	ID         string         `json:"id"`
	URL        string         `json:"url"`
	IngressURL string         `json:"ingress_url"`
	MCP        string         `json:"mcp"`
	Title      string         `json:"title,omitempty"`
	RequestCap int            `json:"request_cap"`
	Provenance provenanceView `json:"provenance"`
	ExpiresAt  time.Time      `json:"expires_at"`
	CreatedAt  time.Time      `json:"created_at"`
	Requests   []requestView  `json:"requests"`
	NextBefore int64          `json:"next_before,omitempty"`
}

// requestView is the JSON view of one captured request's metadata (SPEC-0005
// "Metadata queryable without reading the body"). Body is the raw bytes
// (base64 in JSON, encoding/json's standard []byte encoding) ONLY when the
// body was small enough to stay inline; a spilled body is fetched lazily via
// GET /v1/hooks/{id}/requests/{seq}/body (SPEC-0005 "Bodies MUST be fetched
// lazily when a request is expanded").
type requestView struct {
	Seq         int64               `json:"seq"`
	ReceivedAt  time.Time           `json:"received_at"`
	Method      string              `json:"method"`
	Path        string              `json:"path,omitempty"`
	Query       string              `json:"query,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Status      int                 `json:"status"`
	ContentType string              `json:"content_type,omitempty"`
	BodySize    int64               `json:"body_size"`
	Body        []byte              `json:"body,omitempty"`
	BodyRef     *bodyRefView        `json:"body_ref,omitempty"`
}

// bodyRefView describes a captured request's large body stored as a
// content-addressed blob, fetched lazily via
// GET /v1/hooks/{id}/requests/{seq}/body.
type bodyRefView struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
}

// handleCreateHook provisions a webhook capture endpoint (SPEC-0005 "Webhook
// Endpoint and Two Addresses"). Authenticated owner only.
func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	var req createHookRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, r, err, nil)
		return
	}

	now := s.now()
	ep, err := s.hook.CreateEndpoint(r.Context(), webhook.EndpointInput{
		Title:      req.Title,
		RequestCap: req.RequestCap,
		Provenance: artifact.Provenance{
			ActorID:    p.ActorID,
			OnBehalfOf: req.OnBehalfOf,
			Channel:    p.Channel,
			CapturedAt: now,
		},
		Access:    artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink},
		ExpiresAt: now.Add(s.cfg.DefaultTTL),
	})
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toHookResponse(ep, nil, 0))
}

// handleGetHook fetches an endpoint's metadata plus its recent captured-request
// buffer (first page, newest-first) in one call (SPEC-0005 "GET /v1/hooks/{id}:
// Endpoint metadata + recent request buffer"). Link-capability read: a valid id
// reads, an unknown/expired id is a uniform 404 (ADR-0007).
func (s *Server) handleGetHook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ep, err := s.hook.GetEndpoint(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	page, err := s.hook.ListRequests(r.Context(), id, 0, webhook.DefaultListLimit)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toHookResponse(ep, page.Requests, page.NextBefore))
}

// handleListHookRequests lists an endpoint's captured requests, keyset
// paginated newest-first (SPEC-0005 "GET /v1/hooks/{id}/requests: List
// captured requests (keyset paginated, seq order)"). Link-capability read.
func (s *Server) handleListHookRequests(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, limit, err := parseHookListQuery(r)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	page, err := s.hook.ListRequests(r.Context(), id, before, limit)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	views := make([]requestView, 0, len(page.Requests))
	for _, req := range page.Requests {
		views = append(views, toRequestView(req))
	}
	s.writeJSON(w, http.StatusOK, hookRequestsResponse{Requests: views, NextBefore: page.NextBefore})
}

// hookRequestsResponse is the JSON view of one keyset page of captured
// requests.
type hookRequestsResponse struct {
	Requests   []requestView `json:"requests"`
	NextBefore int64         `json:"next_before,omitempty"`
}

// handleGetHookRequest fetches one captured request's full metadata detail
// (SPEC-0005 "GET /v1/hooks/{id}/requests/{seq}: Full detail of one captured
// request (metadata + body ref)"). Link-capability read.
func (s *Server) handleGetHookRequest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seq, err := parseSeq(r)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	req, err := s.hook.GetRequest(r.Context(), id, seq)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "seq": chi.URLParam(r, "seq")})
		return
	}
	s.writeJSON(w, http.StatusOK, toRequestView(*req))
}

// handleGetHookRequestBody lazily streams a captured request's raw body — the
// deferred fetch the inspector performs only when a request is expanded
// (SPEC-0005 "Bodies MUST be fetched lazily when a request is expanded").
// Link-capability read; a non-executable download disposition mirrors the
// trajectory span-output route (handleGetSpanOutput).
func (s *Server) handleGetHookRequestBody(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seq, err := parseSeq(r)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	rc, info, err := s.hook.OpenRequestBody(r.Context(), id, seq)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "seq": chi.URLParam(r, "seq")})
		return
	}
	defer rc.Close()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(id+"-"+chi.URLParam(r, "seq")+".body"))
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
		s.log.WarnContext(r.Context(), "httpapi: hook request body stream aborted", "error", err)
	}
}

// parseHookListQuery reads and bounds the `before`/`limit` list query params, a
// malformed value being validation_failed rather than silently ignored.
func parseHookListQuery(r *http.Request) (before int64, limit int, err error) {
	if v := r.URL.Query().Get("before"); v != "" {
		before, err = strconv.ParseInt(v, 10, 64)
		if err != nil || before < 0 {
			return 0, 0, errs.Validationf("before must be a non-negative integer seq")
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		var n int
		n, err = strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, errs.Validationf("limit must be a non-negative integer")
		}
		limit = n
	}
	return before, limit, nil
}

// parseSeq parses the {seq} path param as a positive int64, a malformed value
// being validation_failed rather than a uniform-looking not-found.
func parseSeq(r *http.Request) (int64, error) {
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 64)
	if err != nil || seq <= 0 {
		return 0, errs.Validationf("seq must be a positive integer")
	}
	return seq, nil
}

// toHookResponse projects a service Endpoint (plus an already-fetched request
// page) onto its JSON view, building the endpoint's short URL and MCP handle
// from the webhook type's registry URL prefixes (never a switch on the type
// here — ADR-0002/ADR-0005).
func (s *Server) toHookResponse(ep *webhook.Endpoint, reqs []webhook.Request, nextBefore int64) hookResponse {
	pref := s.reg.URLPrefixFor(artifact.TypeWebhook)
	views := make([]requestView, 0, len(reqs))
	for _, req := range reqs {
		views = append(views, toRequestView(req))
	}
	return hookResponse{
		ID:         ep.PublicID,
		URL:        s.cfg.BaseURL + prefixedPath(pref.Web, ep.PublicID),
		IngressURL: s.hookIngressURL(ep.PublicID),
		MCP:        "mcp://cairn" + prefixedPath(pref.MCP, ep.PublicID),
		Title:      ep.Title,
		RequestCap: ep.RequestCap,
		Provenance: provenanceView{
			Actor:      ep.Provenance.ActorID,
			OnBehalfOf: ep.Provenance.OnBehalfOf,
			Channel:    ep.Provenance.Channel,
			CapturedAt: ep.Provenance.CapturedAt,
		},
		ExpiresAt:  ep.ExpiresAt,
		CreatedAt:  ep.CreatedAt,
		Requests:   views,
		NextBefore: nextBefore,
	}
}

// toRequestView projects a service Request onto its JSON view. The body is
// inlined only when it was small enough to stay inline on the row; a spilled
// body carries only its ref, fetched lazily via the dedicated body route.
func toRequestView(r webhook.Request) requestView {
	v := requestView{
		Seq: r.Seq, ReceivedAt: r.ReceivedAt, Method: r.Method, Path: r.Path, Query: r.Query,
		Headers: r.Headers, Status: r.Status, ContentType: r.ContentType, BodySize: r.BodySize,
	}
	if r.Inline != nil {
		v.Body = r.Inline
	}
	if r.Ref != nil {
		v.BodyRef = &bodyRefView{SHA256: r.Ref.SHA256, Size: r.Ref.Size, Truncated: r.Ref.Truncated}
	}
	return v
}
