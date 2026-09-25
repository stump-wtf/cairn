package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/mcpsession"
)

// MCP agent sessions (issue #76, SPEC-0007, ADR-0004): Joe runs multiple
// agents over MCP and wants to make sense of which agent is doing what. A
// session is recorded by the MCP transport (mcp.go) at `initialize` and its
// activity counters incremented on every tool call; this file is the
// owner-scoped read/end surface over that core, mirroring pat.go's
// management-endpoint shape exactly (session/OIDC-authenticated only,
// CSRF-guarded on the mutating call — minting/revoking a connection's
// visibility is a human-at-the-keyboard action, same reasoning as PATs).
//
// Ending a session is not a distinct core operation: it is
// oauth.Service.RevokeGrant on the session's tied grant, the identical
// "revoke anytime in settings" primitive every OAuth connection uses
// (ADR-0004) — this file only resolves the session id to its grant id
// (owner-scoped) before delegating.
//
// Governing: SPEC-0007 (mcp-server-and-oauth), ADR-0004 (subject-vs-actor
// identity mapping, per-connection revocability).

// maxMCPSessionListLimit bounds how far back "recent" reaches on the
// Settings page / GET /v1/mcp/sessions — enough to show a busy human's whole
// day of agent activity without an unbounded query.
const maxMCPSessionListLimit = 50

// mcpSessionView is the JSON view of one recorded MCP session.
type mcpSessionView struct {
	ID                string    `json:"id"`
	ClientName        string    `json:"client_name"`
	ClientVersion     string    `json:"client_version,omitempty"`
	ConnectedAt       time.Time `json:"connected_at"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	ToolCalls         int64     `json:"tool_calls"`
	ArtifactsCreated  int64     `json:"artifacts_created"`
	AnnotationsPosted int64     `json:"annotations_posted"`
	Ended             bool      `json:"ended"`
}

func toMCPSessionView(sess *mcpsession.Session) mcpSessionView {
	return mcpSessionView{
		ID:                sess.ID,
		ClientName:        sess.ClientName,
		ClientVersion:     sess.ClientVersion,
		ConnectedAt:       sess.ConnectedAt,
		LastActivityAt:    sess.LastActivityAt,
		ToolCalls:         sess.ToolCalls,
		ArtifactsCreated:  sess.ArtifactsCreated,
		AnnotationsPosted: sess.AnnotationsPosted,
		Ended:             sess.Ended(),
	}
}

// listMCPSessionsResponse is the GET /v1/mcp/sessions response.
type listMCPSessionsResponse struct {
	Sessions []mcpSessionView `json:"sessions"`
}

// handleListMCPSessions lists the authenticated human's own MCP agent
// sessions (active and recent), most recently active first. Owner-scoped,
// mirroring handleListTokens exactly.
func (s *Server) handleListMCPSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	if s.mcpSessions == nil {
		s.writeJSON(w, http.StatusOK, listMCPSessionsResponse{Sessions: []mcpSessionView{}})
		return
	}
	sessions, err := s.mcpSessions.List(r.Context(), p.UserID, maxMCPSessionListLimit)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	items := make([]mcpSessionView, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, toMCPSessionView(sess))
	}
	s.writeJSON(w, http.StatusOK, listMCPSessionsResponse{Sessions: items})
}

// handleEndMCPSession ends one of the authenticated human's own MCP sessions
// by revoking the OAuth grant it authenticated with (SPEC-0007 REQ "Token
// Revocation": "revoking a grant MUST invalidate that grant's access and
// refresh tokens without affecting the human's other connections") — the
// identical mechanism every other connection uses, so a revoked session's
// agent is cut off exactly like a revoked PAT. An id that does not exist or
// belongs to another owner is a uniform 404, never disclosing another
// owner's sessions.
func (s *Server) handleEndMCPSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	if s.mcpSessions == nil || s.oauth == nil {
		s.writeError(w, r, errs.ErrNotFound, nil)
		return
	}
	id := chi.URLParam(r, "id")
	sess, err := s.mcpSessions.Get(r.Context(), p.UserID, id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	if err := s.oauth.RevokeGrant(r.Context(), sess.GrantID); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.log.InfoContext(r.Context(), "mcp: session ended", "id", id, "owner", p.UserID, "grant_id", sess.GrantID)
	w.WriteHeader(http.StatusNoContent)
}
