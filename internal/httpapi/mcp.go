// The MCP surface (SPEC-0007, ADR-0003, ADR-0004): a Go MCP server exposing
// Cairn's core operations — read, create & push, comment, react — as MCP
// tools, and the trajectory live-span stream as a readable MCP resource,
// mounted in-process at POST/GET /mcp (streamable HTTP transport) alongside
// the REST and web adapters in the same binary. It is a thin adapter: every
// handler resolves the caller's OAuth identity and scope, then calls exactly
// the same core method the REST adapter calls (store, annotation service,
// trajectory service) — ADR-0003 "no surface can fork the rules".
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
	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/store"
	"github.com/joestump/cairn/internal/trajectory"
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
		ResourceMetadataURL: s.cfg.BaseURL + "/.well-known/oauth-protected-resource",
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

// mcpTokenVerifier adapts the OAuth authorization server's AuthenticateAccess
// to the SDK's auth.TokenVerifier seam. Only OAuth-issued access tokens
// authenticate here — the static APIToken table and the dev bearer shortcut
// that back /v1 are deliberately not consulted, so a pre-OAuth static token
// can never reach the MCP transport (SPEC-0007 endpoint table).
func (s *Server) mcpTokenVerifier() sdkauth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		ident, err := s.oauth.AuthenticateAccess(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}
		return &sdkauth.TokenInfo{
			Scopes:     ident.Scopes,
			Expiration: ident.ExpiresAt,
			UserID:     ident.ActorID,
		}, nil
	}
}

// newMCPServer builds the MCP server once, registering the tool and resource
// surface over this adapter's core services.
func (s *Server) newMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "cairn", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Cairn is an AI-native artifact-sharing service. Read and create shareable " +
			"artifacts, comment and react on them, and tail live trajectory runs — all scoped to " +
			"what the authorizing human can already reach.",
		Logger:             s.log,
		SubscribeHandler:   s.mcpSubscribeRun,
		UnsubscribeHandler: s.mcpUnsubscribeRun,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_read",
		Description: "Read an artifact or a named file within a bundle by its public id or mcp://cairn/<id> handle. Requires artifacts:read.",
	}, s.mcpReadArtifact)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "artifact_create",
		Description: "Create and push a new artifact owned by the authorizing human, with the default " +
			"link-visibility policy and default TTL. Requires artifacts:write.",
	}, s.mcpCreateArtifact)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_comment",
		Description: "Post a comment on an artifact (or a one-level reply). Requires annotations:write.",
	}, s.mcpComment)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_react",
		Description: "Add an emoji reaction to an artifact or an anchor within it. Requires annotations:write.",
	}, s.mcpReact)

	if s.traj != nil {
		srv.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: "mcp://cairn/run/{id}",
			Name:        "trajectory-run",
			Description: "A trajectory run's header, derived stats, and ordered span tree. Subscribe to " +
				"receive a notification each time a new span lands. Requires artifacts:read.",
			MIMEType: "application/json",
		}, s.mcpReadRun)
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
	// Body is the artifact content to store.
	Body string `json:"body"`
	// Title is an optional display title.
	Title string `json:"title,omitempty"`
	// MediaType is an optional MIME type; sniffed from the body if omitted.
	MediaType string `json:"media_type,omitempty"`
	// ShareType selects the artifact kind (default "file"); "bundle" and
	// "trajectory" are not creatable through this tool (bundles need N member
	// bodies, trajectories need a span tree — neither fits a single text body).
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
		return nil, mcpCreateOutput{}, fmt.Errorf("validation_failed: share_type must not be %q; use a dedicated flow for bundles and runs", shareType)
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

// runWatch is one live subscription to a run's append stream, forwarding each
// landed span/status transition as an MCP `notifications/resources/updated`
// so a subscribed client knows to re-read the resource (SPEC-0007 "Streaming
// reads MUST deliver incremental data ... as they land").
type runWatch struct {
	cancel context.CancelFunc
}

// mcpRunWatches tracks each (session, run) subscription this server holds, so
// Unsubscribe (or session teardown never firing an explicit unsubscribe, which
// is acceptable: the watch simply outlives an abandoned session until the
// process recycles it) can be torn down precisely without disturbing a
// different session's watch on the same run.
type mcpRunWatches struct {
	mu    sync.Mutex
	watch map[*mcp.ServerSession]map[string]*runWatch // session -> uri -> watch
}

func newMCPRunWatches() *mcpRunWatches {
	return &mcpRunWatches{watch: map[*mcp.ServerSession]map[string]*runWatch{}}
}

var mcpWatches = newMCPRunWatches() //nolint:gochecknoglobals // process-wide registry, mirrors the trajectory hub's own process-wide scope

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

// mcpUnsubscribeRun is the SDK's UnsubscribeHandler hook: it tears down the
// matching watch goroutine started by mcpSubscribeRun.
func (s *Server) mcpUnsubscribeRun(_ context.Context, req *mcp.UnsubscribeRequest) error {
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
