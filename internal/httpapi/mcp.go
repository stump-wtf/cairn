// The MCP surface (SPEC-0007, ADR-0003, ADR-0004): a Go MCP server exposing
// Cairn's core operations — read, create & push (single-body artifacts,
// bundles, and trajectory runs), comment, react — as MCP tools, and the
// trajectory live-span stream as a readable MCP resource, mounted in-process
// at POST/GET /mcp (streamable HTTP transport) alongside the REST and web
// adapters in the same binary. It is a thin adapter: every handler resolves
// the caller's OAuth identity and scope, then calls exactly the same core
// method the REST adapter calls (store, annotation service, trajectory
// service) — ADR-0003 "no surface can fork the rules". artifact_create covers
// the single-body types (file, markdown, code); run_create/run_append_spans
// mirror POST /v1/runs and POST /v1/runs/{id}/spans over the trajectory
// service; bundle_create mirrors the multipart multi-file path the web/CLI
// use over store.CreateBundle — every creatable share type is reachable over
// MCP (issue #65).
//
// Authorization is OAuth-only (SPEC-0007 endpoint table: "/mcp ... Required —
// OAuth 2.1 bearer access token, audience-bound to Cairn"): the static
// APIToken bearer surface and the insecure dev shortcut that authenticate
// /v1 are deliberately NOT wired here, so a static token can never reach the
// MCP transport. The subject of every call is the human the grant was issued
// to (agents inherit, never exceed, the human's reach); the acting model is
// read from the MCP client's `initialize` Implementation and stamped as
// provenance OnBehalfOf with channel `via MCP` (ADR-0004 subject-vs-actor).
//
// Governing: ADR-0003 (triple-surface parity, in-process adapters over one
// core), ADR-0004 (MCP as a first-class surface with OAuth), SPEC-0007 REQ
// "MCP Tool Surface — Artifact & Bundle Read", REQ "Create & Push", REQ
// "Comment & React", REQ "MCP Resource Surface — Stream Reads", REQ
// "Subject/Actor Identity Mapping & Least Privilege", REQ "Error Handling
// Standards", REQ "Rate Limiting", REQ "Request Body Size Limits".
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/mcpsession"
	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/pat"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/store"
	"github.com/joestump/cairn/internal/trajectory"
	"github.com/joestump/cairn/internal/webhook"
)

// maxMCPReadBodyBytes bounds how much of an artifact/member body the read tool
// inlines into a tool result: large enough for the pastebin-shaped text/code
// artifacts the tool targets, small enough not to blow an agent's context.
// Above the bound the body is reported truncated; the full bytes remain
// fetchable over REST/web at the returned url. Governing: SPEC-0007 (MCP is a
// thin adapter — this is a presentation bound, not a core rule).
const maxMCPReadBodyBytes = 1 << 20 // 1 MiB

// maxMCPRequestBytes bounds the raw /mcp POST body before it is buffered by
// the transport, so an oversize request (notably a create-and-push tool call
// whose body argument exceeds the store's own ceiling) is rejected before the
// server reads it into memory (SPEC-0007 REQ "Request Body Size Limits":
// "MCP create-and-push tool bodies, delegating the artifact-body ceiling to
// the core"). JSON-RPC/JSON-string framing overhead is generous slack on top
// of the configured upload ceiling.
func (s *Server) maxMCPRequestBytes() int64 {
	return 2*s.cfg.MaxUploadBytes + (64 << 10)
}

// mcpEnabled reports whether the MCP transport is wired: it requires both the
// core store (read/create/comment/react) and the OAuth authorization server
// (MCP is OAuth-only, SPEC-0007), so a storeless unit wiring mounts nothing.
func (s *Server) mcpEnabled() bool { return s.store != nil && s.oauth != nil }

// mountMCP registers the streamable-HTTP MCP transport at /mcp on the
// strict-CSP API group (SPEC-0007 endpoint table, "strict /v1-style CSP on
// any HTTP-facing MCP endpoint"). The shared per-IP rate limiter already wraps
// every route (Handler); this adds the outer request-size guard and the
// OAuth-only bearer gate in front of the SDK's JSON-RPC dispatch.
func (s *Server) mountMCP(r chi.Router) {
	if !s.mcpEnabled() {
		return
	}
	s.mcpSrv = s.newMCPServer()
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcpSrv }, &mcp.StreamableHTTPOptions{
		Logger: s.log,
	})

	verifier := s.mcpTokenVerifier()
	authed := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		// Point the 401 challenge at the path-specific metadata for this
		// resource (RFC 9728), whose `resource` is the /mcp canonical URI.
		ResourceMetadataURL: s.cfg.BaseURL + "/.well-known/oauth-protected-resource/mcp",
	})(transport)

	r.Handle("/mcp", s.mcpBodyLimit(authed))
}

// mcpBodyLimit rejects an oversize /mcp request before the transport buffers
// it: a declared Content-Length above the ceiling is 413 immediately: a
// chunked/undeclared body is capped defensively via MaxBytesReader as
// defense-in-depth (SPEC-0007 REQ "Request Body Size Limits").
func (s *Server) mcpBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		max := s.maxMCPRequestBytes()
		if r.ContentLength > max {
			s.writeError(w, r, errs.ErrTooLarge, nil)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// mcpExtraGrantID / mcpExtraClientID key the two oauth.Identity fields that
// sdkauth.TokenInfo has no first-class field for (it carries Scopes,
// Expiration, and UserID only) into TokenInfo.Extra, so the initialize
// handler (issue #76: record a session tied to the OAuth grant) can read
// them back off req.Extra.TokenInfo without a second AuthenticateAccess call.
const (
	mcpExtraGrantID  = "grant_id"
	mcpExtraClientID = "client_id"

	// patTokenInfoTTL is the validity window reported for a PAT-authenticated
	// MCP request. PATs never expire, but the bearer middleware reads a zero
	// Expiration as already-expired, so we report a modest future window; the
	// token is re-verified against the DB (including revoked_at) on every
	// request regardless, so revocation still takes effect promptly.
	patTokenInfoTTL = time.Hour
)

// mcpTokenVerifier adapts the OAuth authorization server's AuthenticateAccess
// to the SDK's auth.TokenVerifier seam. Only OAuth-issued access tokens
// authenticate here — the static APIToken table and the dev bearer shortcut
// that back /v1 are deliberately not consulted, so a pre-OAuth static token
// can never reach the MCP transport (SPEC-0007 endpoint table).
func (s *Server) mcpTokenVerifier() sdkauth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		// A personal access token (cairn_pat_) authenticates on the MCP surface
		// too — not just /v1. A PAT is a human-issued, scoped, revocable bearer
		// created in Settings explicitly "for my agents" (#74), and agents connect
		// over MCP, so rejecting it here (issue #51) defeats its purpose. It carries
		// no OAuth grant, so mcpInitializedHandler records no session row (it skips
		// an empty grant_id); the PAT's scopes still gate every tool exactly as an
		// OAuth access token's do. Extends SPEC-0007's OAuth-only MCP surface to the
		// equivalent user-scoped PAT credential; the "cairn_pat_" prefix is disjoint
		// from OAuth's "cairn_at_", so this never misroutes a token. PATs do not
		// expire, so Expiration is set far ahead — a zero value is read as already
		// expired by the bearer middleware.
		if s.pat != nil && pat.IsSecret(token) {
			t, err := s.pat.Authenticate(ctx, token)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
			}
			return &sdkauth.TokenInfo{
				Scopes:     t.Scopes,
				Expiration: s.now().Add(patTokenInfoTTL),
				UserID:     t.OwnerID,
				Extra: map[string]any{
					mcpExtraGrantID:  "",
					mcpExtraClientID: "",
				},
			}, nil
		}
		ident, err := s.oauth.AuthenticateAccess(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}
		return &sdkauth.TokenInfo{
			Scopes:     ident.Scopes,
			Expiration: ident.ExpiresAt,
			UserID:     ident.ActorID,
			Extra: map[string]any{
				mcpExtraGrantID:  ident.GrantID,
				mcpExtraClientID: ident.ClientID,
			},
		}, nil
	}
}

// newMCPServer builds the MCP server once, registering the tool and resource
// surface over this adapter's core services.
func (s *Server) newMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "cairn", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Cairn is an AI-native artifact-sharing service. Read and create shareable " +
			"artifacts, comment and react on them, and tail live trajectory runs and webhook " +
			"streams — all scoped to what the authorizing human can already reach.",
		Logger: s.log,
		// The SDK holds exactly one Subscribe/UnsubscribeHandler pair for the
		// whole server, so mcpSubscribeResource dispatches by URI prefix
		// (mcp://cairn/run/ vs mcp://cairn/hook/) to the per-share-type watch
		// starter; mcpUnsubscribeResource is already share-type-agnostic (keyed
		// purely by session + URI) and serves both (SPEC-0007 REQ "MCP Resource
		// Surface — Stream Reads").
		SubscribeHandler:   s.mcpSubscribeResource,
		UnsubscribeHandler: s.mcpUnsubscribeResource,
		// mcpInitializedHandler records an agent session the moment the
		// client completes the handshake (issue #76: "record an MCP
		// session/connection when an OAuth-authenticated client initializes
		// the MCP transport").
		InitializedHandler: s.mcpInitializedHandler,
	})
	// mcpActivityMiddleware increments a session's activity counters on every
	// tools/call (issue #76: "increment activity counters on tool calls").
	// Receiving middleware wraps every incoming server-bound method, so this
	// runs after the tool handler itself has returned.
	srv.AddReceivingMiddleware(s.mcpActivityMiddleware())

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_read",
		Description: "Read an artifact or a named file within a bundle by its public id or mcp://cairn/<id> handle. Requires artifacts:read.",
	}, s.mcpReadArtifact)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "artifact_create",
		Description: "Create and push a new single-body artifact (file, markdown, or code — share_type " +
			"defaults to file, sniffed/declared media type selects the viewer) owned by the authorizing " +
			"human, with the default link-visibility policy and default TTL. Bundles (N named members) " +
			"are not creatable here — use bundle_create. Trajectory runs (a header + span tree) are not " +
			"creatable here — use run_create. Requires artifacts:write.",
	}, s.mcpCreateArtifact)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_comment",
		Description: "Post a comment on an artifact (or a one-level reply). Requires annotations:write.",
	}, s.mcpComment)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_react",
		Description: "Add an emoji reaction to an artifact or an anchor within it. Requires annotations:write.",
	}, s.mcpReact)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "bundle_create",
		Description: "Create a bundle of N named members (each a body plus an optional media type) " +
			"owned by the authorizing human, with the default link-visibility policy and default TTL " +
			"— the same multi-file create store.CreateBundle performs for the web/CLI. Returns the " +
			"bundle's id, URLs, and member list. Requires artifacts:write.",
	}, s.mcpCreateBundle)

	if s.traj != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name: "run_create",
			Description: "Create a complete trajectory run (a title/prompt/model header plus an ordered " +
				"span tree) owned by the authorizing human, with the default link-visibility policy and " +
				"default TTL — the same ingest POST /v1/runs (mode \"batch\") performs via the trajectory " +
				"service. Returns the run's id, web URL (/run/<id>), and mcp:// handle. Requires artifacts:write.",
		}, s.mcpCreateRun)

		mcp.AddTool(srv, &mcp.Tool{
			Name: "run_append_spans",
			Description: "Append one or more spans to a run this human owns — the same incremental ingest " +
				"POST /v1/runs/{id}/spans performs. Re-posting an already-present span_id is an idempotent " +
				"no-op; a span naming an unknown parent is rejected atomically. Returns the run's current " +
				"state. Requires artifacts:write.",
		}, s.mcpAppendRunSpans)

		// The run_capture prompt (SPEC-0007 REQ "MCP Prompt Surface"). Tool
		// schemas can describe a field but not a workflow: they cannot say
		// "one span per turn", "don't collapse the whole run into five spans",
		// or "pick one category vocabulary and stay in it". Those are the
		// judgments that separate a legible trajectory from a grey bar chart,
		// and prompts are the surface MCP gives us to state them.
		srv.AddPrompt(&mcp.Prompt{
			Name:        "run_capture",
			Title:       "Capture a trajectory run",
			Description: "How to record an agent run as a Cairn trajectory that reads well: span granularity, the category vocabulary, and what to put in each span's output.",
			Arguments: []*mcp.PromptArgument{{
				Name:        "task",
				Title:       "Task",
				Description: "What the run was about, woven into the guidance. Optional.",
			}},
		}, s.mcpRunCapturePrompt)

		srv.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: "mcp://cairn/run/{id}",
			Name:        "trajectory-run",
			Description: "A trajectory run's header, derived stats, and ordered span tree. Subscribe to " +
				"receive a notification each time a new span lands. Requires artifacts:read.",
			MIMEType: "application/json",
		}, s.mcpReadRun)
	}

	if s.hook != nil {
		// The webhook stream is exposed as a READABLE resource only — no tool
		// registers a write path into it (SPEC-0005 "reactions-only", SPEC-0007
		// REQ "MCP Resource Surface — Stream Reads": "The MCP surface MUST
		// provide no write path into a stream in v1"). Reading requires only
		// artifacts:read, the same single scope the trajectory-run resource
		// requires — no fourth scope for streams.
		srv.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: "mcp://cairn/hook/{id}",
			Name:        "webhook-stream",
			Description: "A webhook endpoint's metadata plus its currently-retained captured-request " +
				"buffer (newest first). Subscribe to receive a notification each time a new request is " +
				"captured. Requires artifacts:read.",
			MIMEType: "application/json",
		}, s.mcpReadHook)
	}

	return srv
}

// --- shared helpers -----------------------------------------------------------

// mcpScopes extracts the caller's granted scopes from the token info the
// bearer middleware attached to this call (SPEC-0007 REQ "Subject/Actor
// Identity Mapping & Least Privilege").
func mcpScopes(extra *mcp.RequestExtra) map[string]bool {
	out := map[string]bool{}
	if extra == nil || extra.TokenInfo == nil {
		return out
	}
	for _, sc := range extra.TokenInfo.Scopes {
		out[sc] = true
	}
	return out
}

// mcpActor returns the authenticated human subject — the OAuth grant's actor —
// for this call, or "" if no token info is attached (should not happen behind
// the bearer middleware, guarded defensively by callers).
func mcpActor(extra *mcp.RequestExtra) string {
	if extra == nil || extra.TokenInfo == nil {
		return ""
	}
	return extra.TokenInfo.UserID
}

// mcpModelActor derives the provenance OnBehalfOf value from the connected
// client's `initialize` Implementation (name/version) — the MCP-native
// identification of the acting model/agent, with no extra tool argument
// required of the caller (ADR-0004 "actor is the model").
func mcpModelActor(sess *mcp.ServerSession) string {
	if sess == nil {
		return ""
	}
	params := sess.InitializeParams()
	if params == nil || params.ClientInfo == nil || params.ClientInfo.Name == "" {
		return ""
	}
	if params.ClientInfo.Version == "" {
		return params.ClientInfo.Name
	}
	return params.ClientInfo.Name + "/" + params.ClientInfo.Version
}

// mcpExtraString reads a string value stashed in a TokenInfo.Extra map by
// mcpTokenVerifier (mcpExtraGrantID, mcpExtraClientID) — "" if ti is nil, its
// Extra is nil, or the key is absent/non-string.
func mcpExtraString(ti *sdkauth.TokenInfo, key string) string {
	if ti == nil || ti.Extra == nil {
		return ""
	}
	v, _ := ti.Extra[key].(string)
	return v
}

// --- MCP agent sessions (issue #76, SPEC-0007) ---------------------------------

// mcpInitializedHandler records an MCP agent session the moment the client
// completes the handshake: `initialize` followed by the client's
// `notifications/initialized` (SPEC-0007 acceptance: "Record an MCP
// session/connection when an OAuth-authenticated client initializes the MCP
// transport"). This is the earliest point both facts a session row needs are
// available together: the connecting client's Implementation name/version
// (session.InitializeParams, the same source mcpModelActor draws on) and its
// OAuth identity.
//
// The identity comes from sdkauth.TokenInfoFromContext(ctx), NOT
// req.Extra.TokenInfo: the SDK's own dispatch for this one notification
// (ServerSession.initialized, mcp/server.go) rebuilds the InitializedRequest
// via serverRequestFor, which does not thread Extra through — so
// req.Extra is always nil here, unlike every AddTool handler (which the SDK
// dispatches through the general typed-request path that DOES populate
// Extra from the incoming jsonrpc.Request). ctx, however, IS the same
// request-scoped context RequireBearerToken (auth.go) attached TokenInfo to
// before the streamable transport ever reached the JSON-RPC dispatch, so it
// carries the identity through unaffected by that SDK quirk.
//
// Recording failure is logged, never fatal to the handshake: a session list
// is a presentation aid for Joe, not a capability gate — a DB hiccup here
// must not break a legitimate agent's connection.
func (s *Server) mcpInitializedHandler(ctx context.Context, req *mcp.InitializedRequest) {
	if s.mcpSessions == nil || req == nil || req.Session == nil {
		return
	}
	ti := sdkauth.TokenInfoFromContext(ctx)
	if ti == nil || ti.UserID == "" {
		return
	}
	grantID := mcpExtraString(ti, mcpExtraGrantID)
	if grantID == "" {
		return
	}
	sessionID := req.Session.ID()
	if sessionID == "" {
		// Stateless transports mint no durable session id; there is nothing
		// to key a row on (SPEC-0007's session concept assumes a real
		// streamable-HTTP connection, which mountMCP always wires non-stateless).
		return
	}
	params := req.Session.InitializeParams()
	var clientName, clientVersion string
	if params != nil && params.ClientInfo != nil {
		clientName, clientVersion = params.ClientInfo.Name, params.ClientInfo.Version
	}
	if _, err := s.mcpSessions.Record(ctx, mcpsession.RecordInput{
		ID:            sessionID,
		OwnerID:       ti.UserID,
		GrantID:       grantID,
		ClientID:      mcpExtraString(ti, mcpExtraClientID),
		ClientName:    clientName,
		ClientVersion: clientVersion,
	}); err != nil {
		s.log.WarnContext(ctx, "mcp: record session failed", "error", err)
	}
}

// mcpToolActivityKind classifies a tool name into the activity Touch records
// on a SUCCESSFUL call: the create-and-push tools bump ArtifactsCreated, the
// annotation tools bump AnnotationsPosted, and everything else (artifact_read,
// or a failed call of any tool) is a plain tool-call count.
func mcpToolActivityKind(toolName string) mcpsession.Activity {
	switch toolName {
	case "artifact_create", "bundle_create", "run_create", "run_append_spans":
		return mcpsession.ActivityArtifactCreated
	case "artifact_comment", "artifact_react":
		return mcpsession.ActivityAnnotationPosted
	default:
		return mcpsession.ActivityToolCall
	}
}

// mcpActivityMiddleware is receiving middleware (wraps every incoming
// server-bound JSON-RPC method) that, for a tools/call, touches the calling
// session's activity counters after the tool handler returns (SPEC-0007
// acceptance: "increment activity counters on tool calls"). It runs for
// every tool call regardless of outcome — ToolCalls counts the attempt — and
// additionally bumps the create/annotate counter only when the call actually
// succeeded (no transport error, and the packed CallToolResult is not
// IsError; see mcpScopeErr's doc on how a tool-domain failure is packed).
// Touch itself no-ops on an unrecorded session id (e.g. this process never
// saw that session's `initialize`), so this never fails a tool call over a
// bookkeeping miss.
func (s *Server) mcpActivityMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if s.mcpSessions != nil && method == "tools/call" {
				s.recordMCPToolActivity(ctx, req, result, err)
			}
			return result, err
		}
	}
}

// recordMCPToolActivity is mcpActivityMiddleware's post-call bookkeeping,
// split out so the middleware closure stays a straight-line trace.
func (s *Server) recordMCPToolActivity(ctx context.Context, req mcp.Request, result mcp.Result, callErr error) {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || ss == nil {
		return
	}
	sessionID := ss.ID()
	if sessionID == "" {
		return
	}
	var toolName string
	if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && params != nil {
		toolName = params.Name
	}
	kind := mcpsession.ActivityToolCall
	if callErr == nil {
		if res, ok := result.(*mcp.CallToolResult); ok && res != nil && !res.IsError {
			kind = mcpToolActivityKind(toolName)
		}
	}
	if err := s.mcpSessions.Touch(ctx, sessionID, kind); err != nil {
		s.log.WarnContext(ctx, "mcp: touch session activity failed", "error", err)
	}
}

// mcpScopeErr builds the distinct, stably-coded insufficient_scope failure
// SPEC-0007 requires ("a distinct insufficient_scope error, not a generic
// failure") and logs it structurally (tool + missing scope, never token
// material) before returning it as a tool-domain error: [mcp.AddTool]'s
// generic handler packs a non-nil error into CallToolResult with IsError set,
// which is the standard "failed tool call" shape an agent can see and
// self-correct on — not a JSON-RPC protocol failure.
func (s *Server) mcpScopeErr(ctx context.Context, tool, scope string) error {
	s.log.InfoContext(ctx, "mcp: insufficient scope", "tool", tool, "scope", scope)
	return fmt.Errorf("insufficient_scope: %s requires the %s scope", tool, scope)
}

// mcpToolErr maps a core domain error to the same stable, leak-free code and
// message the REST envelope uses (messageFor/errs.CodeOf), logged
// structurally at info (client-caused) or error (server-caused) level
// (SPEC-0007 REQ "Error Handling Standards": distinguishable typed errors,
// no silent swallowing, no secrets in logs).
func (s *Server) mcpToolErr(ctx context.Context, tool string, err error) error {
	code := errs.CodeOf(err)
	attrs := []any{"tool", tool, "code", code, "error", err}
	if code == errs.CodeInternal {
		s.log.ErrorContext(ctx, "mcp: tool call failed", attrs...)
	} else {
		s.log.InfoContext(ctx, "mcp: tool call rejected", attrs...)
	}
	return fmt.Errorf("%s: %s", code, messageFor(code))
}

// normalizeMCPHandle resolves a bare public id or an mcp://cairn/<id> agent
// handle (optionally under its /run/ or /hook/ sub-prefix) to the bare public
// id every core lookup takes (ADR-0005: "one id, every surface").
func normalizeMCPHandle(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "mcp://cairn/")
	s = strings.TrimPrefix(s, "run/")
	s = strings.TrimPrefix(s, "hook/")
	return s
}

// --- artifact_read -------------------------------------------------------------

type mcpReadInput struct {
	// ID is the artifact/bundle/run public id, or an mcp://cairn/<id> handle
	// (ADR-0005).
	ID string `json:"id"`
	// Path names a file within a bundle; omitted for a non-bundle artifact or
	// to list a bundle's members.
	Path string `json:"path,omitempty"`
}

type mcpMemberView struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type mcpReadOutput struct {
	artifactResponse
	// Body is the artifact's (or named member's) content, inlined when it fits
	// within maxMCPReadBodyBytes.
	Body string `json:"body,omitempty"`
	// BodyEncoding is "utf8" or "base64", set whenever Body is populated.
	BodyEncoding string `json:"body_encoding,omitempty"`
	// BodyTruncated reports whether Body holds only a prefix of the full
	// content — the full bytes remain fetchable at URL.
	BodyTruncated bool `json:"body_truncated,omitempty"`
	// Members lists a bundle's files when Path was not given.
	Members []mcpMemberView `json:"members,omitempty"`
}

// mcpReadArtifact is the artifact_read tool handler (SPEC-0007 REQ "MCP Tool
// Surface — Artifact & Bundle Read"): it resolves the same core read the REST
// GET /v1/artifacts/{id} (and bundle member) handlers call, returning a
// uniform not-found for an unknown, unauthorized, or expired id.
//
// Governing: SPEC-0007 REQ "MCP Tool Surface — Artifact & Bundle Read",
// ADR-0007 (uniform not-found), ADR-0003 (thin adapter over the core).
func (s *Server) mcpReadArtifact(ctx context.Context, req *mcp.CallToolRequest, in mcpReadInput) (*mcp.CallToolResult, mcpReadOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, mcpReadOutput{}, s.mcpScopeErr(ctx, "artifact_read", oauth.ScopeArtifactsRead)
	}
	id := normalizeMCPHandle(in.ID)
	if id == "" {
		return nil, mcpReadOutput{}, fmt.Errorf("validation_failed: id is required")
	}
	art, err := s.store.GetByPublicID(ctx, id)
	if err != nil {
		return nil, mcpReadOutput{}, s.mcpToolErr(ctx, "artifact_read", err)
	}
	out := mcpReadOutput{artifactResponse: s.toArtifactResponse(art)}

	if art.ShareType == artifact.TypeBundle && in.Path == "" {
		members, err := s.store.ListMembers(ctx, id)
		if err != nil {
			return nil, mcpReadOutput{}, s.mcpToolErr(ctx, "artifact_read", err)
		}
		out.Members = make([]mcpMemberView, 0, len(members))
		for _, m := range members {
			out.Members = append(out.Members, mcpMemberView{Name: m.Name, Size: m.Size, MediaType: m.MediaType})
		}
		return nil, out, nil
	}

	var (
		rc   io.ReadCloser
		info store.BodyInfo
	)
	if in.Path != "" {
		rc, info, err = s.store.OpenMember(ctx, id, in.Path)
	} else {
		rc, info, err = s.store.OpenBody(ctx, id)
	}
	if err != nil {
		return nil, mcpReadOutput{}, s.mcpToolErr(ctx, "artifact_read", err)
	}
	defer rc.Close()
	// A bundle member's size/media type are the member's own, not the
	// bundle artifact's (which carries no single body) — override.
	if in.Path != "" {
		out.Size = info.Size
		out.MediaType = info.MediaType
		out.Checksum = info.SHA256
	}

	// Read up to the inline cap plus one byte, so reading exactly cap+1 bytes
	// (n == len(buf)) distinguishes "truncated" from "the whole body fit".
	buf := make([]byte, maxMCPReadBodyBytes+1)
	n, readErr := io.ReadFull(rc, buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return nil, mcpReadOutput{}, s.mcpToolErr(ctx, "artifact_read", fmt.Errorf("read body: %w", readErr))
	}
	truncated := n > maxMCPReadBodyBytes
	if truncated {
		n = maxMCPReadBodyBytes
	}
	body := buf[:n]
	out.BodyTruncated = truncated || info.Size > int64(n)
	if utf8.Valid(body) {
		out.Body = string(body)
		out.BodyEncoding = "utf8"
	} else {
		out.Body = base64.StdEncoding.EncodeToString(body)
		out.BodyEncoding = "base64"
	}
	return nil, out, nil
}

// --- artifact_create -----------------------------------------------------------

type mcpCreateInput struct {
	// Body is the artifact content to store. It is written to the store
	// VERBATIM as the JSON string's bytes — there is no base64/binary decoding
	// path yet, so an image (or any other genuinely binary) body created this
	// way would be corrupted (the JSON string is UTF-8 text, not an arbitrary
	// byte string) unless a caller supplies a body that happens to already be
	// valid text. This is a known, documented gap (issue #69's follow-up on
	// #65): a model actor can post an image_region annotation once the image
	// artifact already exists (annotations are plain JSON, no binary
	// involved), but creating the IMAGE artifact itself over MCP needs its own
	// base64-body (or a dedicated upload-and-return-a-handle) tool — tracked
	// as a follow-up rather than folded into this signature, since it is a
	// breaking-ish addition (a new encoding field) better landed with its own
	// story. The web upload path (POST /v1/artifacts with a raw or multipart
	// binary body) is unaffected and is the supported way to create an image
	// artifact today.
	Body string `json:"body"`
	// Title is an optional display title.
	Title string `json:"title,omitempty"`
	// MediaType is an optional MIME type; sniffed from the body if omitted.
	MediaType string `json:"media_type,omitempty"`
	// ShareType selects the artifact kind (default "file"); "bundle" and
	// "trajectory" are not creatable through this tool (bundles need N member
	// bodies, trajectories need a span tree — neither fits a single text body).
	// "image" is technically accepted, but see the Body field's doc comment:
	// there is no binary-safe path yet, so this only works for text bodies a
	// generous media-type sniff happens to accept.
	ShareType string `json:"share_type,omitempty"`
}

type mcpCreateOutput struct {
	artifactResponse
}

// mcpCreateArtifact is the artifact_create tool handler (SPEC-0007 REQ
// "Create & Push"): it creates a new artifact owned by the human subject via
// the same core create operation the REST/web/CLI surfaces call, with the
// default link-visibility policy and default TTL always applied — the input
// schema (struct-derived, additionalProperties:false) carries no field able to
// request a broader policy, TTL, or owner, so a request to broaden access is
// rejected by schema validation before this handler ever runs (SPEC-0007
// scenario "Create attempts a non-default policy"). Provenance stamps the
// human subject as actor, the MCP client's identification as OnBehalfOf, and
// channel `via MCP` (ADR-0004).
func (s *Server) mcpCreateArtifact(ctx context.Context, req *mcp.CallToolRequest, in mcpCreateInput) (*mcp.CallToolResult, mcpCreateOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsWrite] {
		return nil, mcpCreateOutput{}, s.mcpScopeErr(ctx, "artifact_create", oauth.ScopeArtifactsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpCreateOutput{}, s.mcpToolErr(ctx, "artifact_create", errs.ErrUnauthorized)
	}
	if in.Body == "" {
		return nil, mcpCreateOutput{}, fmt.Errorf("validation_failed: body must not be empty")
	}
	shareType := artifact.ShareType(firstNonEmpty(in.ShareType, string(artifact.TypeFile)))
	if shareType == artifact.TypeBundle || shareType == artifact.TypeTrajectory {
		return nil, mcpCreateOutput{}, fmt.Errorf("validation_failed: share_type must not be %q; use bundle_create for bundles or run_create for trajectory runs", shareType)
	}
	now := s.now()
	art, err := s.store.CreateArtifact(ctx, store.CreateArtifactInput{
		ShareType:         shareType,
		Title:             in.Title,
		Body:              strings.NewReader(in.Body),
		DeclaredMediaType: in.MediaType,
		Provenance: artifact.Provenance{
			ActorID:    actorID,
			OnBehalfOf: mcpModelActor(req.Session),
			Channel:    artifact.ChannelMCP,
			CapturedAt: now,
		},
		Access:    artifact.AccessPolicy{OwnerID: actorID, Visibility: artifact.VisibilityLink},
		ExpiresAt: now.Add(s.cfg.DefaultTTL),
	})
	if err != nil {
		return nil, mcpCreateOutput{}, s.mcpToolErr(ctx, "artifact_create", err)
	}
	return nil, mcpCreateOutput{artifactResponse: s.toArtifactResponse(art)}, nil
}

// --- bundle_create ---------------------------------------------------------------

// mcpBundleMemberInput is one named file to place in a bundle over MCP: a
// body plus an optional declared media type (sniffed like any other artifact
// body when omitted). Unlike artifact_read's inline body cap, there is no
// size ceiling applied here beyond the ordinary upload ceiling the store
// itself enforces per member.
type mcpBundleMemberInput struct {
	// Name is the member's file name within the bundle (must be unique).
	Name string `json:"name"`
	// Body is the member's content.
	Body string `json:"body"`
	// MediaType is an optional MIME type; sniffed from the body if omitted.
	MediaType string `json:"media_type,omitempty"`
}

type mcpBundleCreateInput struct {
	// Title is an optional display title for the bundle.
	Title string `json:"title,omitempty"`
	// Members are the bundle's named files, in bundle order.
	Members []mcpBundleMemberInput `json:"members"`
}

type mcpBundleCreateOutput struct {
	artifactResponse
	// Members lists the created bundle's files in bundle order.
	Members []mcpMemberView `json:"members,omitempty"`
}

// mcpCreateBundle is the bundle_create tool handler (SPEC-0007 REQ "Create &
// Push", issue #65): it creates a bundle of N named members via the same
// core store.CreateBundle the web/CLI multipart path calls, with the default
// link-visibility policy and default TTL always applied — the input schema
// (struct-derived, additionalProperties:false) carries no field able to
// request a broader policy, TTL, or owner, matching artifact_create's
// not-broadening contract. Provenance stamps the human subject as actor, the
// MCP client's identification as OnBehalfOf, and channel `via MCP`
// (ADR-0004). Member validation (non-empty name, uniqueness, at least one
// member) is the core store's own — never duplicated here — so a bundle
// created over MCP obeys the identical invariants as one created over the
// web/CLI.
func (s *Server) mcpCreateBundle(ctx context.Context, req *mcp.CallToolRequest, in mcpBundleCreateInput) (*mcp.CallToolResult, mcpBundleCreateOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsWrite] {
		return nil, mcpBundleCreateOutput{}, s.mcpScopeErr(ctx, "bundle_create", oauth.ScopeArtifactsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpBundleCreateOutput{}, s.mcpToolErr(ctx, "bundle_create", errs.ErrUnauthorized)
	}
	members := make([]store.MemberInput, 0, len(in.Members))
	for _, m := range in.Members {
		members = append(members, store.MemberInput{
			Name:              m.Name,
			Body:              strings.NewReader(m.Body),
			DeclaredMediaType: m.MediaType,
		})
	}
	now := s.now()
	art, err := s.store.CreateBundle(ctx, store.CreateBundleInput{
		Title:   in.Title,
		Members: members,
		Provenance: artifact.Provenance{
			ActorID:    actorID,
			OnBehalfOf: mcpModelActor(req.Session),
			Channel:    artifact.ChannelMCP,
			CapturedAt: now,
		},
		Access:    artifact.AccessPolicy{OwnerID: actorID, Visibility: artifact.VisibilityLink},
		ExpiresAt: now.Add(s.cfg.DefaultTTL),
	})
	if err != nil {
		return nil, mcpBundleCreateOutput{}, s.mcpToolErr(ctx, "bundle_create", err)
	}
	out := mcpBundleCreateOutput{artifactResponse: s.toArtifactResponse(art)}
	created, err := s.store.ListMembers(ctx, art.PublicID)
	if err != nil {
		return nil, mcpBundleCreateOutput{}, s.mcpToolErr(ctx, "bundle_create", err)
	}
	out.Members = make([]mcpMemberView, 0, len(created))
	for _, m := range created {
		out.Members = append(out.Members, mcpMemberView{Name: m.Name, Size: m.Size, MediaType: m.MediaType})
	}
	return nil, out, nil
}

// --- run_capture prompt -------------------------------------------------------

// catList renders a category vocabulary as a comma-separated list for the
// prompt text, derived from the trajectory package's own slices so the prompt
// can never advertise a vocabulary the palette and the schema don't share.
func catList(cats []trajectory.Category) string {
	names := make([]string, 0, len(cats))
	for _, c := range cats {
		names = append(names, string(c))
	}
	return strings.Join(names, ", ")
}

// mcpRunCapturePrompt serves the `run_capture` prompt: the workflow-level
// guidance a per-field schema cannot carry.
//
// It exists because of a real failure. A client ingested a twelve-span run with
// invented categories and no `output` on any span, and the viewer rendered it
// exactly as ingested: a uniformly grey waterfall of rows that expanded onto
// nothing. Every individual field had been filled in plausibly. What was
// missing was the shape of a good capture, which is what this prompt states.
//
// The text is deliberately prescriptive and ordered by how badly each mistake
// degrades the render — output first, since omitting it is both the most common
// error and the one that empties the page.
//
// Governing: ADR-0004 (MCP as a first-class surface), ADR-0009 (open category
// set), SPEC-0004 (Trajectory Share), SPEC-0007 REQ "MCP Prompt Surface".
func (s *Server) mcpRunCapturePrompt(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	task := ""
	if req != nil && req.Params != nil {
		task = req.Params.Arguments["task"]
	}
	intro := "You are recording an agent run to Cairn as a trajectory, using the run_create tool (or run_create followed by run_append_spans for a run you capture as it happens)."
	if strings.TrimSpace(task) != "" {
		intro += " The run to capture is: " + task
	}

	body := intro + `

A trajectory is read by a human scrubbing a waterfall and expanding the turns that look interesting. Optimize for that reader.

**First: find your own transcript and capture from THAT, not from memory.** You are almost certainly running inside a harness that already keeps a high-fidelity local record of this session — per-turn timestamps, every tool call with its arguments, every result, and token usage. That record IS the trajectory; your job is mostly to reshape it. Reconstructing from what you remember yields round-numbered guesses and a waterfall whose shape is fiction, and it is not obvious to the reader that it is fiction. Parsing the transcript costs one read and a little scripting.

Go and look before you write a single span. Two confirmed shapes, as worked examples:
  - Claude Code — ` + "`~/.claude/projects/<cwd-slug>/<session-id>.jsonl`" + `, one JSON object per line, each carrying a ` + "`timestamp`" + ` and a ` + "`message`" + ` whose ` + "`content`" + ` holds ` + "`text`" + ` / ` + "`tool_use`" + ` / ` + "`tool_result`" + ` blocks, plus ` + "`usage`" + ` for tokens.
  - Crush — a SQLite database at ` + "`~/.crush/crush.db`" + `: table ` + "`messages`" + ` with ` + "`role`" + `, millisecond ` + "`created_at`" + ` / ` + "`finished_at`" + `, and a JSON ` + "`parts`" + ` column holding ` + "`reasoning`" + ` and ` + "`tool_call`" + ` parts (each with ` + "`name`" + ` and ` + "`input`" + `).

If yours is neither, it still almost certainly exists — find it rather than assuming it does not. Look under the harness's data or state directory (` + "`~/.<harness>/`" + `, ` + "`~/.local/share/<harness>/`" + `, ` + "`~/Library/Application Support/<harness>/`" + `, or a ` + "`.<harness>/`" + ` folder in the project), and prefer the newest file, or the one whose name carries the current session id. A JSONL of turns and a SQLite database are the two common shapes; both are readable with tools you already have.

From whatever you find, derive each span's ` + "`start_offset_ms`" + ` and ` + "`duration_ms`" + ` from real timestamps (a tool call runs from its invocation to its matching result), the ` + "`tool`" + ` name and ` + "`args`" + ` from the call itself, the ` + "`output`" + ` from the result, and ` + "`token_count`" + ` by summing usage. If you truly cannot find a transcript, say so plainly in your reply to the human and label the run's timings as estimated — a trajectory that looks measured but is not is worse than one that admits it is approximate.

**Put the content in ` + "`output`" + `.** This is the single most important thing, and the most commonly skipped. Every span's ` + "`output`" + ` is what a reader sees when they expand it. For a toolless turn, that is the reasoning/thinking text for that step. For a tool call, it is the stdout or the result. A span sent with only a name and a duration renders as an empty row — the viewer says so explicitly, because there is nothing else it can show. Never pre-truncate: large outputs are stored as blobs and fetched lazily.

**One span per turn, not per phase.** Aim for the granularity you actually worked at — a span for each reasoning turn and each tool call. Collapsing an hour of work into five summary spans throws away exactly the detail the viewer exists to show. A long run with a hundred spans reads fine; a five-span run reads like a summary someone already wrote.

**Pick one category vocabulary and stay in it for the whole run.** Mixing the two reads as noise, because they answer different questions.
  - Operation kind — what you did in the span: ` + catList(trajectory.OperationCategories) + `
  - Workflow phase — where in the job the span sat: ` + catList(trajectory.PhaseCategories) + `

A category outside both is accepted and rendered with a color hashed from its name, so your own vocabulary works. Prefer the lists above when they fit: they are hand-colored, and phases that mean the same thing as an operation share its hue.

**Set ` + "`tool`" + ` on tool calls, and leave it off reasoning turns.** A ` + "`reason`" + ` span must not carry a tool. Use the literal ` + "`sub-agent`" + ` for a nested agent excursion, and give its children that span's ` + "`span_id`" + ` as their ` + "`parent_span_id`" + ` — they then render nested under it.

**Send ` + "`args`" + ` on tool calls.** They render above the output on expand, and they are usually what makes a tool call legible ("which file?", "which command?"). Omit them on spans that take no arguments.

**Get the timing right.** ` + "`start_offset_ms`" + ` is measured from the run's ` + "`started_at`" + `, and ` + "`duration_ms`" + ` is real elapsed time — read both from the transcript rather than estimating. The waterfall lays spans out on these, not on arrival order, so approximating them flattens the shape of the run — the parallelism, the long poll, the slow build. Time spent waiting on a human is real elapsed time too: give it a ` + "`wait`" + ` span rather than folding it into the neighbouring turn, and the same for a step blocked on CI or a rate limit.

**Fill in the run header.** ` + "`title`" + ` (short and specific), ` + "`prompt`" + ` (the human's originating request — it becomes the opening turn), ` + "`model`" + `, and ` + "`token_count`" + `, which cannot be derived from the spans.

Send spans as a flat list; nesting is expressed through ` + "`parent_span_id`" + `, never by nesting the JSON.`

	return &mcp.GetPromptResult{
		Description: "Guidance for capturing an agent run as a well-formed Cairn trajectory.",
		Messages: []*mcp.PromptMessage{{
			Role:    "user",
			Content: &mcp.TextContent{Text: body},
		}},
	}, nil
}

// --- run_create / run_append_spans ------------------------------------------------

// mcpRunSpanInput mirrors [spanRequest], but is shaped for the AGENT rather
// than for wire-symmetry with REST. Two fields differ, both because a
// []byte-backed Go type infers as a JSON *array of 0-255 integers*:
//
//   - Args is `any` (not json.RawMessage), the same substitution
//     [mcpAnchorInput] makes for its anchor_ref, so it surfaces as an embedded
//     JSON object rather than a byte array.
//   - Output is `string` (not []byte), carrying the span's content as PLAIN
//     UTF-8 TEXT.
//
// Output was []byte until it was tested end-to-end over a real MCP session,
// which showed the field was effectively unusable. The inferred schema was
// `{"type":["null","array"],"items":{"type":"integer","minimum":0,"maximum":255}}`,
// so the base64 string REST accepts was REJECTED by schema validation, and the
// only accepted encoding was a JSON array of byte integers — roughly 4-6x the
// bytes, for text an agent already holds as a string. Any agent reading that
// schema would reasonably skip the field, which is part of why runs arrived
// with no output at all. The comment this replaces claimed REST and MCP shared
// "the identical wire contract"; they did not.
//
// This deliberately breaks wire symmetry with REST, and that is the point: MCP
// is the surface agents write to and must be shaped for them, while REST serves
// humans and deterministic clients and keeps []byte/base64. ADR-0003's parity
// is about CAPABILITY — every surface can do the same things — not about
// byte-identical encodings (SPEC-0007 REQ "Agent-Shaped Tool Schemas").
// Non-UTF-8 output is the one thing this cannot carry; a span whose output is
// genuinely binary belongs on the REST ingest.
//
// Every field carries a `jsonschema` description. That is not decoration: an
// agent only ever sees this schema, and with bare types it has no way to know
// that `category` has a recommended vocabulary or that `output` is the thing the
// viewer reveals on expand. Runs ingested before these descriptions existed
// arrived with invented categories and no output at all, and rendered as a grey
// timeline of rows that expanded onto nothing. mcpRunSpanSchemaDoc pins the
// category list to trajectory.RecommendedCategories so the two cannot drift.
type mcpRunSpanInput struct {
	SpanID       string `json:"span_id" jsonschema:"Stable identifier for this span, unique within the run. Re-posting an existing span_id to run_append_spans is an idempotent no-op."`
	ParentSpanID string `json:"parent_span_id,omitempty" jsonschema:"span_id of the enclosing span, for a child of a sub-agent excursion. Omit for a top-level span. Naming a span that is not in the run rejects the whole batch."`
	Category     string `json:"category" jsonschema:"What this span was doing. Sets the waterfall color and the time-by-category breakdown. Any non-empty string is accepted, but pick ONE vocabulary and use it for the whole run. Operation kind: reason, net, exec, read, write, search, plan, tool, analyze, test, fix, fail, meta. Workflow phase: research, implementation, review, testing, debug, build, docs, delivery, deploy, wait. A category outside both still renders, colored from a hash of its name."`
	Name         string `json:"name,omitempty" jsonschema:"Short human label for the span, shown in the waterfall row and the stream header, e.g. 'Explore templates and CSS' or 'go test ./internal/httpapi'."`
	Tool         string `json:"tool,omitempty" jsonschema:"Name of the tool this span invoked, e.g. 'bash', 'read_file', 'web_fetch'. Omit for a pure reasoning turn; a 'reason' span must not carry one. Use the literal 'sub-agent' for a nested agent excursion, whose children reference this span's span_id as their parent_span_id."`
	Args         any    `json:"args,omitempty" jsonschema:"The structured arguments this span was invoked with, as a JSON object. Rendered above the output when a reader expands the span. Omit for a span that takes no arguments — an empty object renders as nothing."`
	// Output is the single most consequential field on this struct and the one
	// agents most often omit, so its description says outright what happens when
	// it is missing.
	Output             string `json:"output,omitempty" jsonschema:"The span's content as plain text: the reasoning/thinking text for a toolless turn, or the stdout/result for a tool call. THIS IS WHAT A READER SEES WHEN THEY EXPAND THE SPAN — a span sent without output renders as an empty row. Send it verbatim; do not base64-encode it and do not pre-truncate, since oversized outputs are stored as blobs and fetched lazily by the viewer. If you did truncate, set output_truncated."`
	OutputTruncated    bool   `json:"output_truncated,omitempty" jsonschema:"Set when output is already a truncated prefix of a larger result, so the viewer labels it truncated rather than implying it is complete."`
	StartOffsetMS      int    `json:"start_offset_ms" jsonschema:"Milliseconds from the run's started_at to when this span began. The waterfall lays spans out on this, not on arrival order."`
	DurationMS         int    `json:"duration_ms" jsonschema:"How long this span took, in milliseconds. Sets the bar width and feeds the time-by-category totals."`
	ProducedArtifactID string `json:"produced_artifact_id,omitempty" jsonschema:"Public id of a Cairn artifact this span produced. Renders the span as a link to that artifact."`
}

// toRunSpanInputs converts the MCP wire spans to the trajectory service's
// SpanInput values, re-marshaling each span's Args back to the
// json.RawMessage the core trajectory service expects (the inverse of
// [mcpAnchorInput.anchorRefJSON], same reasoning).
func toRunSpanInputs(tool string, spans []mcpRunSpanInput) ([]trajectory.SpanInput, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	out := make([]trajectory.SpanInput, 0, len(spans))
	for _, sp := range spans {
		var args json.RawMessage
		if sp.Args != nil {
			b, err := json.Marshal(sp.Args)
			if err != nil {
				return nil, fmt.Errorf("validation_failed: %s: span %q args is not valid JSON", tool, sp.SpanID)
			}
			args = b
		}
		out = append(out, trajectory.SpanInput{
			SpanID:       sp.SpanID,
			ParentSpanID: sp.ParentSpanID,
			Category:     trajectory.Category(sp.Category),
			Name:         sp.Name,
			Tool:         sp.Tool,
			Args:         args,
			// The agent sends text; the service stores bytes. Converting here
			// keeps the MCP schema agent-shaped without the trajectory service
			// growing a second, string-flavoured ingest path.
			Output:             []byte(sp.Output),
			OutputTruncated:    sp.OutputTruncated,
			StartOffsetMS:      sp.StartOffsetMS,
			DurationMS:         sp.DurationMS,
			ProducedArtifactID: sp.ProducedArtifactID,
		})
	}
	return out, nil
}

// mcpRunOutput mirrors [runResponse]: every field is identical to what GET
// /v1/runs/{id} (and the trajectory-run MCP resource) already returns
// (ADR-0003 parity), EXCEPT Spans, which cannot reuse [spanView] directly —
// see the field doc.
type mcpRunOutput struct {
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
	// Spans is the ordered span tree, wire-identical to [spanView] (each span's
	// args/output_ref/children included), but held as `any` rather than
	// []spanView for two independent reasons: (1) spanView.Args is a
	// json.RawMessage, the same byte-array-schema misinference [mcpAnchorInput]
	// works around for AnchorRef — Args needs to surface as an embedded JSON
	// object, not a base64 string; (2) spanView.Children is self-referencing,
	// and the MCP SDK's reflection-based output-schema builder rejects a
	// recursive struct outright ("cycle detected for type ..."), which a typed
	// mirror struct (even a distinctly-named one) would reproduce identically.
	// `any` sidesteps both: the schema is unrestricted, and the value is built
	// by marshaling []spanView then unmarshaling into `any`, so json.RawMessage
	// decodes to a plain object and there is no Go type for the schema builder
	// to recurse into.
	Spans any `json:"spans"`
}

// toMCPRunOutput projects a REST [runResponse] (already built by
// [Server.toRunResponse]) onto the MCP-shaped output, round-tripping Spans
// through JSON once for the reasons documented on [mcpRunOutput.Spans].
func toMCPRunOutput(r runResponse) (mcpRunOutput, error) {
	out := mcpRunOutput{
		ID: r.ID, URL: r.URL, MCP: r.MCP, Status: r.Status, Title: r.Title, Prompt: r.Prompt,
		Model: r.Model, TokenCount: r.TokenCount, Provenance: r.Provenance, StartedAt: r.StartedAt,
		EndedAt: r.EndedAt, ExpiresAt: r.ExpiresAt, Stats: r.Stats,
	}
	if len(r.Spans) == 0 {
		return out, nil
	}
	b, err := json.Marshal(r.Spans)
	if err != nil {
		return mcpRunOutput{}, fmt.Errorf("internal: encode run spans: %w", err)
	}
	var spans any
	if err := json.Unmarshal(b, &spans); err != nil {
		return mcpRunOutput{}, fmt.Errorf("internal: decode run spans: %w", err)
	}
	out.Spans = spans
	return out, nil
}

// mcpRunCreateInput mirrors [runRequest] minus Mode (a run created over MCP
// is always a complete batch run, exactly POST /v1/runs mode "batch") and
// minus OnBehalfOf (derived from the connected client's `initialize`
// identity via [mcpModelActor], the same MCP-native provenance source every
// other write tool uses — never a client-supplied claim over this transport).
type mcpRunCreateInput struct {
	Title      string            `json:"title,omitempty" jsonschema:"Short title for the run, shown in the page header and any listing, e.g. 'msgbrowse #227: semantic status-banner component'."`
	Prompt     string            `json:"prompt,omitempty" jsonschema:"The human's originating request, rendered as the opening turn of the activity stream."`
	Model      string            `json:"model,omitempty" jsonschema:"Model identifier that produced the run, e.g. 'claude-opus-5'. Shown in the trace header."`
	TokenCount int64             `json:"token_count,omitempty" jsonschema:"Total tokens the run consumed. Shown as a run stat; it cannot be derived from the spans."`
	StartedAt  time.Time         `json:"started_at,omitempty" jsonschema:"RFC 3339 timestamp for the start of the run. Every span's start_offset_ms is measured from here. Defaults to now."`
	Spans      []mcpRunSpanInput `json:"spans,omitempty" jsonschema:"The run's spans, as a flat list; nesting is expressed through parent_span_id, not by nesting the JSON. Order does not matter — the viewer orders by start_offset_ms."`
}

// mcpCreateRun is the run_create tool handler (SPEC-0007 REQ "Create & Push",
// issue #65): it ingests a complete run — header plus ordered span tree — via
// the same trajectory.Service.CreateBatchRun the REST POST /v1/runs (mode
// "batch") handler calls, with the default link-visibility policy and
// default TTL always applied (struct-derived input schema,
// additionalProperties:false, carries no policy/TTL/owner field — matching
// artifact_create's not-broadening contract). Provenance stamps the human
// subject as actor, the MCP client's identification as OnBehalfOf, and
// channel `via MCP` (ADR-0004).
func (s *Server) mcpCreateRun(ctx context.Context, req *mcp.CallToolRequest, in mcpRunCreateInput) (*mcp.CallToolResult, mcpRunOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsWrite] {
		return nil, mcpRunOutput{}, s.mcpScopeErr(ctx, "run_create", oauth.ScopeArtifactsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_create", errs.ErrUnauthorized)
	}
	spans, err := toRunSpanInputs("run_create", in.Spans)
	if err != nil {
		return nil, mcpRunOutput{}, err
	}
	now := s.now()
	started := in.StartedAt
	if started.IsZero() {
		started = now
	}
	run, err := s.traj.CreateBatchRun(ctx, trajectory.RunInput{
		Title:      in.Title,
		Prompt:     in.Prompt,
		Model:      in.Model,
		TokenCount: in.TokenCount,
		StartedAt:  started,
		Provenance: artifact.Provenance{
			ActorID:    actorID,
			OnBehalfOf: mcpModelActor(req.Session),
			Channel:    artifact.ChannelMCP,
			CapturedAt: now,
		},
		Access:    artifact.AccessPolicy{OwnerID: actorID, Visibility: artifact.VisibilityLink},
		ExpiresAt: now.Add(s.cfg.DefaultTTL),
		Spans:     spans,
	})
	if err != nil {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_create", err)
	}
	out, err := toMCPRunOutput(s.toRunResponse(run))
	if err != nil {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_create", err)
	}
	return nil, out, nil
}

// mcpRunAppendInput mirrors [appendSpansRequest] plus the run id every tool
// call needs (the REST shape carries the id in the URL path instead).
type mcpRunAppendInput struct {
	// ID is the run's public id, or an mcp://cairn/run/<id> handle.
	ID    string            `json:"id" jsonschema:"The run's public id, or its mcp://cairn/run/<id> handle."`
	Spans []mcpRunSpanInput `json:"spans" jsonschema:"Spans to append. Re-posting a span_id already in the run is an idempotent no-op, so a retry after a failed call is safe."`
}

// mcpAppendRunSpans is the run_append_spans tool handler (SPEC-0007 REQ
// "Create & Push", issue #65 "optional but nice"): it appends spans to an
// open run this human owns via the same trajectory.Service.AppendSpans the
// REST POST /v1/runs/{id}/spans handler calls — owner-only, additive,
// idempotent on a re-posted span_id, atomic on a malformed tree — returning
// the run's current state so batch and incremental read back identically
// (SPEC-0004 "Batch and incremental converge").
func (s *Server) mcpAppendRunSpans(ctx context.Context, req *mcp.CallToolRequest, in mcpRunAppendInput) (*mcp.CallToolResult, mcpRunOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsWrite] {
		return nil, mcpRunOutput{}, s.mcpScopeErr(ctx, "run_append_spans", oauth.ScopeArtifactsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_append_spans", errs.ErrUnauthorized)
	}
	id := normalizeMCPHandle(in.ID)
	spans, err := toRunSpanInputs("run_append_spans", in.Spans)
	if err != nil {
		return nil, mcpRunOutput{}, err
	}
	if _, err := s.traj.AppendSpans(ctx, id, actorID, spans); err != nil {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_append_spans", err)
	}
	run, err := s.traj.GetRun(ctx, id)
	if err != nil {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_append_spans", err)
	}
	out, err := toMCPRunOutput(s.toRunResponse(run))
	if err != nil {
		return nil, mcpRunOutput{}, s.mcpToolErr(ctx, "run_append_spans", err)
	}
	return nil, out, nil
}

// --- artifact_comment / artifact_react ------------------------------------------

// mcpAnchorInput is the anchor shape shared by the comment and react tools.
// AnchorRef is `any` (not json.RawMessage) so the input schema stays
// unrestricted — an arbitrary JSON object/scalar — rather than the byte-array
// schema []byte would otherwise infer; the handler re-marshals it to the
// json.RawMessage the annotation core expects.
type mcpAnchorInput struct {
	// ID is the artifact/run public id, or an mcp://cairn/<id> handle.
	ID string `json:"id"`
	// AnchorType is the anchor kind, e.g. "artifact" for the whole artifact,
	// or a type-specific anchor (md_block, code_line, text_selection, ...).
	// It is registry-validated per share type (ADR-0006); there is no
	// implicit default, matching the REST surface's identical contract.
	AnchorType string `json:"anchor_type"`
	// AnchorRef is the anchor locator object, shaped per anchor_type.
	AnchorRef any `json:"anchor_ref,omitempty"`
}

func (a mcpAnchorInput) anchorRefJSON() (json.RawMessage, error) {
	if a.AnchorRef == nil {
		return nil, nil
	}
	b, err := json.Marshal(a.AnchorRef)
	if err != nil {
		return nil, fmt.Errorf("validation_failed: anchor_ref is not valid JSON")
	}
	return b, nil
}

type mcpCommentInput struct {
	mcpAnchorInput
	// ParentID threads a reply under a root comment (omit for a root).
	ParentID *int64 `json:"parent_id,omitempty"`
	// Body is the comment text.
	Body string `json:"body"`
}

// mcpCommentOutput mirrors [commentResponse] but carries AnchorRef as `any`
// rather than json.RawMessage: a []byte-backed type infers as a JSON *array*
// schema (bytes), which then fails client-side output validation against the
// object/scalar the anchor ref actually is. `any` infers as unrestricted and
// still marshals the underlying json.RawMessage verbatim (json.Marshal honors
// json.Marshaler through an interface value), so the wire shape is identical.
type mcpCommentOutput struct {
	ID         int64      `json:"id"`
	AnchorType string     `json:"anchor_type"`
	AnchorRef  any        `json:"anchor_ref,omitempty"`
	AnchorKey  string     `json:"anchor_key"`
	ParentID   *int64     `json:"parent_id,omitempty"`
	ActorID    string     `json:"actor_id"`
	OnBehalfOf string     `json:"on_behalf_of,omitempty"`
	Body       string     `json:"body"`
	CreatedAt  time.Time  `json:"created_at"`
	EditedAt   *time.Time `json:"edited_at,omitempty"`
	Deleted    bool       `json:"deleted"`
}

func toMCPCommentOutput(c annotation.Comment) mcpCommentOutput {
	r := toCommentResponse(c)
	return mcpCommentOutput{
		ID: r.ID, AnchorType: r.AnchorType, AnchorRef: r.AnchorRef, AnchorKey: r.AnchorKey,
		ParentID: r.ParentID, ActorID: r.ActorID, OnBehalfOf: r.OnBehalfOf, Body: r.Body,
		CreatedAt: r.CreatedAt, EditedAt: r.EditedAt, Deleted: r.Deleted,
	}
}

// mcpComment is the artifact_comment tool handler (SPEC-0007 REQ "Comment &
// React"): it posts a comment via the same annotation core the REST adapter
// calls, with the anchor validated by the share-type registry (ADR-0006) and
// provenance stamping the model actor + channel `via MCP`.
func (s *Server) mcpComment(ctx context.Context, req *mcp.CallToolRequest, in mcpCommentInput) (*mcp.CallToolResult, mcpCommentOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeAnnotationsWrite] {
		return nil, mcpCommentOutput{}, s.mcpScopeErr(ctx, "artifact_comment", oauth.ScopeAnnotationsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpCommentOutput{}, s.mcpToolErr(ctx, "artifact_comment", errs.ErrUnauthorized)
	}
	id := normalizeMCPHandle(in.ID)
	ref, err := in.anchorRefJSON()
	if err != nil {
		return nil, mcpCommentOutput{}, err
	}
	comment, err := s.annot.AddComment(ctx, id, annotation.CommentInput{
		AnchorType: sharetype.Anchor(in.AnchorType),
		AnchorRef:  ref,
		ParentID:   in.ParentID,
		ActorID:    actorID,
		OnBehalfOf: mcpModelActor(req.Session),
		Body:       in.Body,
	})
	if err != nil {
		return nil, mcpCommentOutput{}, s.mcpToolErr(ctx, "artifact_comment", err)
	}
	return nil, toMCPCommentOutput(comment), nil
}

type mcpReactInput struct {
	mcpAnchorInput
	// Emoji is a single emoji to react with.
	Emoji string `json:"emoji"`
}

// mcpReactOutput mirrors [reactionResponse] with the same AnchorRef `any`
// substitution as [mcpCommentOutput], for the identical schema-inference
// reason.
type mcpReactOutput struct {
	ID         int64     `json:"id"`
	AnchorType string    `json:"anchor_type"`
	AnchorRef  any       `json:"anchor_ref,omitempty"`
	Emoji      string    `json:"emoji"`
	ActorID    string    `json:"actor_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func toMCPReactOutput(r annotation.Reaction) mcpReactOutput {
	v := toReactionResponse(r)
	return mcpReactOutput{
		ID: v.ID, AnchorType: v.AnchorType, AnchorRef: v.AnchorRef, Emoji: v.Emoji,
		ActorID: v.ActorID, CreatedAt: v.CreatedAt,
	}
}

// mcpReact is the artifact_react tool handler (SPEC-0007 REQ "Comment &
// React"): it records an idempotent reaction via the same annotation core the
// REST adapter calls, with provenance stamping the model actor + channel
// `via MCP`. React carries no on_behalf_of field on the wire today (REST
// parity: [reactionRequest] has none either), so the model actor is recorded
// implicitly by the channel alone.
func (s *Server) mcpReact(ctx context.Context, req *mcp.CallToolRequest, in mcpReactInput) (*mcp.CallToolResult, mcpReactOutput, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeAnnotationsWrite] {
		return nil, mcpReactOutput{}, s.mcpScopeErr(ctx, "artifact_react", oauth.ScopeAnnotationsWrite)
	}
	actorID := mcpActor(req.Extra)
	if actorID == "" {
		return nil, mcpReactOutput{}, s.mcpToolErr(ctx, "artifact_react", errs.ErrUnauthorized)
	}
	id := normalizeMCPHandle(in.ID)
	ref, err := in.anchorRefJSON()
	if err != nil {
		return nil, mcpReactOutput{}, err
	}
	reaction, _, err := s.annot.React(ctx, id, sharetype.Anchor(in.AnchorType), ref, in.Emoji, actorID)
	if err != nil {
		return nil, mcpReactOutput{}, s.mcpToolErr(ctx, "artifact_react", err)
	}
	return nil, toMCPReactOutput(reaction), nil
}

// --- trajectory run resource (SPEC-0007 REQ "MCP Resource Surface — Stream Reads") ---

// mcpReadRun is the ResourceHandler for the mcp://cairn/run/{id} template: a
// read returns the run's current header, derived stats, and ordered span tree
// — the identical projection [runResponse] the REST GET /v1/runs/{id} handler
// renders (ADR-0003 parity) — as JSON text content. Reading requires only
// artifacts:read; there is no write path (SPEC-0007: "streams are read-only to
// agents in v1").
func (s *Server) mcpReadRun(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read run", oauth.ScopeArtifactsRead)
	}
	id, ok := matchRunURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a run resource URI", req.Params.URI)
	}
	run, err := s.traj.GetRun(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run", err)
	}
	body, err := json.Marshal(s.toRunResponse(run))
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run", fmt.Errorf("encode run: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
	}}, nil
}

// matchRunURI extracts the run id from a resolved mcp://cairn/run/<id> URI.
func matchRunURI(uri string) (string, bool) {
	const prefix = "mcp://cairn/run/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(uri, prefix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// runWatch is one live subscription to a trajectory run's append stream or a
// webhook endpoint's capture stream, forwarding each landed span/status
// transition or captured request as an MCP `notifications/resources/updated`
// so a subscribed client knows to re-read the resource (SPEC-0007 "Streaming
// reads MUST deliver incremental data ... as they land"). The name predates
// the webhook stream resource; the type itself is share-type-agnostic (just a
// cancel func) and is shared by both mcpSubscribeRun and mcpSubscribeHook.
type runWatch struct {
	cancel context.CancelFunc
}

// mcpRunWatches tracks each (session, URI) subscription this server holds —
// across BOTH the trajectory-run and webhook-stream resources, keyed purely
// by URI so it needs no per-share-type registry — so Unsubscribe (or session
// teardown never firing an explicit unsubscribe, which is acceptable: the
// watch simply outlives an abandoned session until the process recycles it)
// can be torn down precisely without disturbing a different session's watch
// on the same resource.
type mcpRunWatches struct {
	mu    sync.Mutex
	watch map[*mcp.ServerSession]map[string]*runWatch // session -> uri -> watch
}

func newMCPRunWatches() *mcpRunWatches {
	return &mcpRunWatches{watch: map[*mcp.ServerSession]map[string]*runWatch{}}
}

var mcpWatches = newMCPRunWatches() //nolint:gochecknoglobals // process-wide registry, mirrors the trajectory/webhook hubs' own process-wide scope

// mcpSubscribeRun is the SDK's SubscribeHandler hook: it starts a background
// watch on the run's append stream (via the trajectory hub the SSE endpoint
// already uses) and calls Server.ResourceUpdated on every new span or status
// transition, so a subscribed MCP client is notified live without a fourth
// scope or a write path into the stream.
func (s *Server) mcpSubscribeRun(ctx context.Context, req *mcp.SubscribeRequest) error {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return s.mcpScopeErr(ctx, "resources/subscribe run", oauth.ScopeArtifactsRead)
	}
	id, ok := matchRunURI(req.Params.URI)
	if !ok {
		return fmt.Errorf("validation_failed: %q is not a run resource URI", req.Params.URI)
	}
	if s.traj == nil {
		return fmt.Errorf("not_found: not found or expired")
	}
	sess := req.Session
	uri := req.Params.URI
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sub := s.traj.Subscribe(id)

	mcpWatches.mu.Lock()
	if mcpWatches.watch[sess] == nil {
		mcpWatches.watch[sess] = map[string]*runWatch{}
	}
	if existing, dup := mcpWatches.watch[sess][uri]; dup {
		existing.cancel()
	}
	mcpWatches.watch[sess][uri] = &runWatch{cancel: cancel}
	mcpWatches.mu.Unlock()

	go func() {
		defer sub.Close()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-sub.Lagged():
				return
			case ev, ok := <-sub.Events():
				if !ok {
					return
				}
				switch ev.Type {
				case trajectory.EventSpan, trajectory.EventStatus:
					_ = s.mcpSrv.ResourceUpdated(watchCtx, &mcp.ResourceUpdatedNotificationParams{URI: uri})
				}
				// A closed run publishes no further events — stop watching
				// rather than idling a goroutine and a hub subscriber slot
				// forever on an abandoned subscription.
				if ev.Type == trajectory.EventStatus && ev.Status == trajectory.StatusClosed {
					return
				}
			}
		}
	}()
	return nil
}

// mcpUnsubscribeResource is the SDK's single, share-type-agnostic
// UnsubscribeHandler hook: it tears down the matching watch goroutine started
// by mcpSubscribeRun or mcpSubscribeHook. It never needs to know which share
// type a URI names — mcpWatches is keyed purely by (session, URI) — so it
// serves both the trajectory-run and webhook-stream resources without a
// dispatch.
func (s *Server) mcpUnsubscribeResource(_ context.Context, req *mcp.UnsubscribeRequest) error {
	sess := req.Session
	uri := req.Params.URI
	mcpWatches.mu.Lock()
	defer mcpWatches.mu.Unlock()
	if byURI, ok := mcpWatches.watch[sess]; ok {
		if w, ok := byURI[uri]; ok {
			w.cancel()
			delete(byURI, uri)
		}
		if len(byURI) == 0 {
			delete(mcpWatches.watch, sess)
		}
	}
	return nil
}

// --- webhook stream resource (SPEC-0007 REQ "MCP Resource Surface — Stream Reads") ---

// mcpReadHook is the ResourceHandler for the mcp://cairn/hook/{id} template:
// a read returns the endpoint's metadata plus its currently-retained
// captured-request buffer — the identical projection [hookResponse] the REST
// GET /v1/hooks/{id} handler renders (ADR-0003 parity) — as JSON text
// content. Reading requires only artifacts:read; there is no write path
// (SPEC-0005 "Ingress grants no read" — nor does any authenticated read
// surface let an agent push into the stream; SPEC-0007: "streams are
// read-only to agents in v1").
func (s *Server) mcpReadHook(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read hook", oauth.ScopeArtifactsRead)
	}
	id, ok := matchHookURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a webhook resource URI", req.Params.URI)
	}
	ep, err := s.hook.GetEndpoint(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read hook", err)
	}
	page, err := s.hook.ListRequests(ctx, id, 0, webhook.DefaultListLimit)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read hook", err)
	}
	body, err := json.Marshal(s.toHookResponse(ep, page.Requests, page.NextBefore))
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read hook", fmt.Errorf("encode hook: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
	}}, nil
}

// matchHookURI extracts the endpoint id from a resolved
// mcp://cairn/hook/<id> URI.
func matchHookURI(uri string) (string, bool) {
	const prefix = "mcp://cairn/hook/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(uri, prefix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// mcpSubscribeResource is the SDK's single SubscribeHandler hook, dispatching
// by URI prefix to the trajectory or webhook watch starter — the SDK holds
// exactly one handler pair for the whole server, so the multiplex lives here
// rather than in a per-resource-template hook (SPEC-0007 REQ "MCP Resource
// Surface — Stream Reads").
func (s *Server) mcpSubscribeResource(ctx context.Context, req *mcp.SubscribeRequest) error {
	uri := req.Params.URI
	switch {
	case strings.HasPrefix(uri, "mcp://cairn/run/"):
		return s.mcpSubscribeRun(ctx, req)
	case strings.HasPrefix(uri, "mcp://cairn/hook/"):
		return s.mcpSubscribeHook(ctx, req)
	default:
		return fmt.Errorf("validation_failed: %q is not a subscribable resource", uri)
	}
}

// mcpSubscribeHook is the webhook half of mcpSubscribeResource: it starts a
// background watch on the endpoint's capture stream (via the same
// webhook.Service hub the SSE endpoint uses, internal/httpapi/hook_stream.go)
// and calls Server.ResourceUpdated on every newly captured request, so a
// subscribed MCP client is notified live without a fourth scope or a write
// path into the stream (SPEC-0007: "Streaming reads MUST deliver incremental
// data ... as they arrive").
func (s *Server) mcpSubscribeHook(ctx context.Context, req *mcp.SubscribeRequest) error {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return s.mcpScopeErr(ctx, "resources/subscribe hook", oauth.ScopeArtifactsRead)
	}
	id, ok := matchHookURI(req.Params.URI)
	if !ok {
		return fmt.Errorf("validation_failed: %q is not a webhook resource URI", req.Params.URI)
	}
	if s.hook == nil {
		return fmt.Errorf("not_found: not found or expired")
	}
	sess := req.Session
	uri := req.Params.URI
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sub := s.hook.Subscribe(id)

	mcpWatches.mu.Lock()
	if mcpWatches.watch[sess] == nil {
		mcpWatches.watch[sess] = map[string]*runWatch{}
	}
	if existing, dup := mcpWatches.watch[sess][uri]; dup {
		existing.cancel()
	}
	mcpWatches.watch[sess][uri] = &runWatch{cancel: cancel}
	mcpWatches.mu.Unlock()

	go func() {
		defer sub.Close()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-sub.Lagged():
				return
			case ev, ok := <-sub.Events():
				if !ok {
					return
				}
				if ev.Type == webhook.EventRequest {
					_ = s.mcpSrv.ResourceUpdated(watchCtx, &mcp.ResourceUpdatedNotificationParams{URI: uri})
				}
			}
		}
	}()
	return nil
}
