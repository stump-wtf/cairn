package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/pat"
)

// Personal access tokens (PATs, issue #74): human-minted bearer credentials
// for the /v1 surface, created and revoked from the web Settings UI. A PAT is
// a third implementation of the ADR-0004 auth seam — alongside the static
// CAIRN_API_TOKENS env credential (TokenAuthenticator) and OAuth 2.1 access
// tokens (OAuthAuthenticator) — so a PAT authenticates on /v1 exactly like a
// CAIRN_API_TOKENS entry once minted (PATAuthenticator below), while the
// management endpoints (create/list/revoke) are core operations the /v1
// Settings surface adapts, not a bespoke mechanism.
//
// Governing: ADR-0004 (token seam), SPEC-0007 (three-scope model, hashed-at-
// rest secrets).

// maxPATRequestBytes bounds the create-token JSON body before it is
// buffered — the payload is a name and a short scope list, never large
// (SPEC-0007 REQ "Request Body Size Limits").
const maxPATRequestBytes = 4 << 10

// --- Management endpoints (session-authenticated, CSRF-guarded) -------------

// createTokenRequest is the POST /v1/tokens body.
type createTokenRequest struct {
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
	IsAgent bool     `json:"is_agent"`
}

// tokenView is the metadata-only JSON view of a token — the secret is never
// present here (issue #74 acceptance: "list, metadata only — never the
// secret").
type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	IsAgent    bool       `json:"is_agent"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// createTokenResponse is the POST /v1/tokens response: the metadata view plus
// the ONE-TIME plaintext secret. No other endpoint ever returns it again.
type createTokenResponse struct {
	tokenView
	Token string `json:"token"`
}

// listTokensResponse is the GET /v1/tokens response.
type listTokensResponse struct {
	Tokens []tokenView `json:"tokens"`
}

func toTokenView(t *pat.Token) tokenView {
	return tokenView{
		ID:         t.ID,
		Name:       t.Name,
		Scopes:     t.Scopes,
		IsAgent:    t.IsAgent,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
	}
}

// handleCreateToken mints a new PAT owned by the authenticated human and
// returns its plaintext secret exactly once (issue #74: "One-time plaintext
// shown only at creation"). scopes MUST be a subset of the three ADR-0004
// consent scopes; an empty scopes list defaults to all three, matching the
// OAuth consent convention (oauth.ParseScope) this package reuses.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPATRequestBytes)
	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, r, errs.ErrTooLarge, nil)
			return
		}
		s.writeError(w, r, errs.Validationf("tokens: malformed JSON body"), nil)
		return
	}
	scopes, err := pat.ParseScope(strings.Join(req.Scopes, " "))
	if err != nil {
		s.writeError(w, r, errs.Validationf("tokens: scopes must be a subset of: %s", pat.JoinScope(pat.AllScopes())), nil)
		return
	}
	secret, tok, err := s.pat.Create(r.Context(), p.ActorID, strings.TrimSpace(req.Name), scopes, req.IsAgent)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	s.log.InfoContext(r.Context(), "pat: token created",
		"id", tok.ID, "owner", tok.OwnerID, "is_agent", tok.IsAgent, "scope", pat.JoinScope(tok.Scopes))
	s.writeJSON(w, http.StatusCreated, createTokenResponse{tokenView: toTokenView(tok), Token: secret})
}

// handleListTokens lists the authenticated human's own tokens, metadata only.
// Owner-scoped: a human only ever sees their own tokens (issue #74
// acceptance: "Owner-scoped: a human only sees/manages their own").
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	toks, err := s.pat.List(r.Context(), p.ActorID)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	items := make([]tokenView, 0, len(toks))
	for _, t := range toks {
		items = append(items, toTokenView(t))
	}
	s.writeJSON(w, http.StatusOK, listTokensResponse{Tokens: items})
}

// handleRevokeToken revokes one of the authenticated human's own tokens. An
// id that does not exist, belongs to another owner, or is already revoked is
// a uniform 404 — the endpoint never discloses another owner's tokens.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.pat.Revoke(r.Context(), p.ActorID, id); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.log.InfoContext(r.Context(), "pat: token revoked", "id", id, "owner", p.ActorID)
	w.WriteHeader(http.StatusNoContent)
}

// requireHumanSession is middleware for the token-management endpoints
// (POST/GET/DELETE /v1/tokens): only an authenticated, non-agent WEB SESSION
// principal may create, list, or revoke personal access tokens (issue #74:
// "session/OIDC-authenticated"). A bearer-token caller (a PAT, an OAuth agent
// token, or a static CAIRN_API_TOKENS entry) is refused here even if
// otherwise valid — minting or revoking credentials is a human-at-the-
// keyboard action, mirroring the same Ambient+!IsAgent gate the OAuth consent
// screen uses (sessionPrincipal, oauth.go) for the identical reason.
func (s *Server) requireHumanSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.sessionPrincipal(r)
		if !ok {
			s.writeError(w, r, errs.ErrUnauthorized, nil)
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- Bearer authentication over issued personal access tokens ---------------

// PATAuthenticator resolves `Authorization: Bearer <token>` against minted
// personal access tokens, wiring PATs into the same bearer chain as the
// static CAIRN_API_TOKENS entries and OAuth access tokens (ADR-0004: "wire
// PATs into the existing bearer auth chain ... so a PAT authenticates on /v1
// exactly like a CAIRN_API_TOKENS entry"). The subject (ActorID) is always
// the owning human; IsAgent is the token's own flag, which keeps
// human-only capabilities (delete; httpapi.requireHuman) off an agent-marked
// PAT regardless of which of the three scopes it holds — sharing:manage is
// never grantable to any PAT at all, since pat.ParseScope only recognizes the
// three ADR-0004 scopes.
type PATAuthenticator struct {
	svc *pat.Service
}

// Authenticate implements Authenticator. Any failure — unknown or revoked
// secret — is a uniform errs.ErrUnauthorized, matching the other bearer
// authenticators' fail-closed shape.
func (a *PATAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := bearerToken(r)
	if token == "" || a.svc == nil {
		return nil, errs.ErrUnauthorized
	}
	tok, err := a.svc.Authenticate(r.Context(), token)
	if err != nil {
		return nil, errs.ErrUnauthorized
	}
	scopes := make(map[string]bool, len(tok.Scopes))
	for _, sc := range tok.Scopes {
		scopes[sc] = true
	}
	return &Principal{
		ActorID: tok.OwnerID,
		Channel: artifact.ChannelAPI,
		IsAgent: tok.IsAgent,
		Scopes:  scopes,
	}, nil
}
