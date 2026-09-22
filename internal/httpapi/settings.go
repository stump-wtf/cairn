package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/mcpsession"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/pat"
)

// The Settings page (issue #75): a real, sectioned settings surface —
// API tokens, MCP connection, CLI, Account — replacing the standalone
// "Connect an agent" page (#57). It is server-rendered exactly like the Bin
// (ADR-0011): the initial GET renders the signed-in actor's current token
// list from the same PAT core the JSON /v1/tokens endpoints (issue #74) use,
// so the no-JS page already shows every fact except the freshly-minted
// secret. Creating and revoking a token are JS-enhanced actions that call
// those SAME /v1/tokens endpoints directly from settings.js (fetch + the
// double-submit CSRF header), rather than a bespoke web-only mutation path —
// there is exactly one implementation of "mint/revoke a PAT" in the codebase
// (pat.Service), and exactly one HTTP surface for it (pat.go's handlers);
// this page only adds a view over the same calls a scripted /v1 caller makes.
//
// Governing: SPEC-0001 REQ "Settings & Connect Instructions" (web app shell),
// SPEC-0007 (three-scope PAT vocabulary, reused verbatim via oauth.ConsentLine
// so scope copy never drifts between the OAuth consent screen and the Settings
// token-creation form), ADR-0004 (one token seam), ADR-0011 (server-rendered
// shell + CSP-build Alpine/HTMX).

// settingsView is the Settings page's view model: the signed-in identity +
// sign-in method (Account section), the CSRF token the header sign-out form
// and settings.js's fetch calls both need, the deployment's public base URL
// the MCP section's copy-paste steps are built from (never hardcoded, so a
// differently-deployed instance shows its own origin — matching the old
// connectView), the grantable scope vocabulary for the token-creation
// checkboxes, and the actor's current tokens.
type settingsView struct {
	Actor        string
	AuthMethod   string // "Pocket ID (OIDC)" or "dev password" — how this session was established
	CSRFToken    string
	BaseURL      string
	ScopeOptions []scopeOptionView
	Tokens       []tokenRowView
	// MCPSessions lists this human's connected/recent agents (issue #76) —
	// which client (name + version), when it connected, when it last acted,
	// and its activity counts, so Joe can tell his agents apart.
	MCPSessions []mcpSessionRowView
	// Grants lists the human's live OAuth connections (A20, issue #343) —
	// clients authorized over REST or MCP alike, each revocable here.
	Grants []grantRowView
}

// grantsResponse is the GET /v1/oauth/grants body: the same rows the
// settings page renders, for settings.js's list-refresh path.
type grantsResponse struct {
	Grants []grantRowView `json:"grants"`
}

// scopeOptionView is one checkbox in the token-creation form: the wire scope
// value plus its plain-language label, sourced from oauth.ConsentLine so the
// exact same three descriptions appear here and on the OAuth consent screen
// (SPEC-0007 REQ "Exactly Three Consent Scopes" — one vocabulary, two forms).
type scopeOptionView struct {
	Value string
	Label string
}

// tokenRowView is one row in the API tokens list: metadata only, matching the
// JSON tokenView (pat.go) the settings.js list-refresh path re-renders from —
// the secret is never present here, whether server- or client-rendered
// (issue #74 acceptance: "list, metadata only — never the secret").
type tokenRowView struct {
	ID         string
	Name       string
	ScopeLabel string // space-joined scopes, e.g. "artifacts:read artifacts:write"
	IsAgent    bool
	Created    string // humanized
	LastUsed   string // humanized, or "never"
	Revoked    bool
	RevokedAt  string // humanized, empty unless Revoked
}

// toTokenRowView projects a pat.Token into its display row.
func toTokenRowView(t *pat.Token) tokenRowView {
	row := tokenRowView{
		ID:         t.ID,
		Name:       t.Name,
		ScopeLabel: pat.JoinScope(t.Scopes),
		IsAgent:    t.IsAgent,
		Created:    humanizeSince(t.CreatedAt),
		LastUsed:   "never",
	}
	if t.LastUsedAt != nil {
		row.LastUsed = humanizeSince(*t.LastUsedAt)
	}
	if t.RevokedAt != nil {
		row.Revoked = true
		row.RevokedAt = humanizeSince(*t.RevokedAt)
	}
	return row
}

// mcpSessionRowView is one row in the Agent sessions list (issue #76): the
// client identity from the MCP `initialize` handshake, when it connected and
// last acted, and its activity counts — matching the JSON mcpSessionView
// (mcpsessions.go) settings.js's revoke-refresh path reads from, the same
// server/client-shape parity tokenRowView keeps with tokenView.
type mcpSessionRowView struct {
	ID                string
	ClientName        string
	ClientVersion     string
	Connected         string // humanized
	LastActivity      string // humanized
	ToolCalls         int64
	ArtifactsCreated  int64
	AnnotationsPosted int64
	Ended             bool
}

// toMCPSessionRowView projects an mcpsession.Session into its display row.
func toMCPSessionRowView(sess *mcpsession.Session) mcpSessionRowView {
	name := sess.ClientName
	if name == "" {
		name = "unknown client"
	}
	return mcpSessionRowView{
		ID:                sess.ID,
		ClientName:        name,
		ClientVersion:     sess.ClientVersion,
		Connected:         humanizeSince(sess.ConnectedAt),
		LastActivity:      humanizeSince(sess.LastActivityAt),
		ToolCalls:         sess.ToolCalls,
		ArtifactsCreated:  sess.ArtifactsCreated,
		AnnotationsPosted: sess.AnnotationsPosted,
		Ended:             sess.Ended(),
	}
}

// scopeOptions builds the token-creation form's checkbox vocabulary from the
// canonical three-scope list (oauth.AllScopes), reusing oauth.ConsentLine so
// this form and the OAuth consent screen can never describe a scope
// differently.
func scopeOptions() []scopeOptionView {
	all := oauth.AllScopes()
	out := make([]scopeOptionView, 0, len(all))
	for _, sc := range all {
		out = append(out, scopeOptionView{Value: sc, Label: oauth.ConsentLine(sc)})
	}
	return out
}

// handleSettingsPage renders the Settings page (issue #75): API tokens, MCP
// connection, CLI, and Account, gated exactly like the Bin (requireWebSession
// — an anonymous request never reaches here, it is redirected to login with a
// validated ?next). The token list is read fresh on every load from the same
// owner-scoped PAT.List the JSON GET /v1/tokens endpoint calls, so a
// server-rendered reload and a settings.js fetch refresh can never disagree.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	vm := settingsView{
		Actor:        p.ActorID,
		AuthMethod:   s.authMethodLabel(),
		BaseURL:      s.cfg.BaseURL,
		ScopeOptions: scopeOptions(),
	}
	if c, cerr := r.Cookie(csrfCookieName); cerr == nil {
		vm.CSRFToken = c.Value
	}
	if s.pat != nil {
		toks, err := s.pat.List(r.Context(), p.ActorID)
		if err != nil {
			s.log.WarnContext(r.Context(), "settings: list tokens failed", "error", err)
		} else {
			vm.Tokens = make([]tokenRowView, 0, len(toks))
			for _, t := range toks {
				vm.Tokens = append(vm.Tokens, toTokenRowView(t))
			}
		}
	}
	if s.mcpSessions != nil {
		sessions, err := s.mcpSessions.List(r.Context(), p.ActorID, maxMCPSessionListLimit)
		if err != nil {
			s.log.WarnContext(r.Context(), "settings: list mcp sessions failed", "error", err)
		} else {
			vm.MCPSessions = make([]mcpSessionRowView, 0, len(sessions))
			for _, sess := range sessions {
				vm.MCPSessions = append(vm.MCPSessions, toMCPSessionRowView(sess))
			}
		}
	}
	if s.oauth != nil {
		grants, err := s.oauth.ListActorGrants(r.Context(), p.ActorID)
		if err != nil {
			s.log.WarnContext(r.Context(), "settings: list oauth grants failed", "error", err)
		} else {
			vm.Grants = make([]grantRowView, 0, len(grants))
			for _, g := range grants {
				vm.Grants = append(vm.Grants, toGrantRowView(g))
			}
		}
	}
	s.renderWeb(w, r, "settings", vm)
}

// authMethodLabel names how the current session was established, for the
// Account section's identity card. Pocket ID (OIDC) is the deployed default
// (ADR-0013); the dev password is the local-dev-only fallback (loginEnabled).
// It reports the deployment's configured method, not a fact resolved from the
// individual session — Cairn has exactly one login path enabled at a time.
func (s *Server) authMethodLabel() string {
	if s.oidc != nil {
		return "Pocket ID (OIDC)"
	}
	return "dev password"
}

// handleConnectRedirect absorbs the retired standalone "Connect an agent"
// page (#57) into the Settings page's MCP connection section (issue #75): a
// permanent redirect, since the URL itself moved rather than the capability
// changing. It is unconditional — the redirect happens before any auth check,
// exactly like any other moved path — /settings itself (requireWebSession)
// is what sends an anonymous caller on to login.
func (s *Server) handleConnectRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings", http.StatusMovedPermanently)
}

// grantRowView is one row in the OAuth connections list (A20, issue #343):
// the client that holds a grant acting as this human, with its scopes and
// when it was last used — metadata only, like tokenRowView. These are
// grants issued over REST (and any other surface), which the Agent sessions
// table cannot show: only MCP activity opens a session row there.
type grantRowView struct {
	ID       string
	Client   string
	Scope    string
	Created  string // humanized
	LastUsed string // humanized, or "never"
}

// toGrantRowView projects an oauth.ActorGrant into its display row.
func toGrantRowView(g *oauth.ActorGrant) grantRowView {
	row := grantRowView{
		ID:       g.GrantID,
		Client:   g.Client,
		Scope:    g.Scope,
		Created:  humanizeSince(g.CreatedAt),
		LastUsed: "never",
	}
	if g.LastUsed != nil {
		row.LastUsed = humanizeSince(*g.LastUsed)
	}
	return row
}

// handleListGrants lists the authenticated human's live OAuth grants
// (A20, SPEC-0023 REQ "Closing the Audited Surfaces"). Session-authenticated
// only (requireHumanSession refuses every bearer caller), like the token and
// MCP session management routes — a bearer caller revoking credentials is
// the finding itself.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	if s.oauth == nil {
		s.writeError(w, r, errs.ErrNotFound, nil)
		return
	}
	grants, err := s.oauth.ListActorGrants(r.Context(), p.ActorID)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	rows := make([]grantRowView, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, toGrantRowView(g))
	}
	s.writeJSON(w, http.StatusOK, grantsResponse{Grants: rows})
}

// handleRevokeGrant revokes one of the authenticated human's own OAuth
// grants and its whole token family: the connected agent's next call fails
// immediately, on every surface the grant worked on. Unknown or foreign ids
// are a uniform 404 — the endpoint never discloses another actor's grants.
func (s *Server) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	if s.oauth == nil {
		s.writeError(w, r, errs.ErrNotFound, nil)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.oauth.RevokeActorGrant(r.Context(), p.ActorID, id); err != nil {
		s.writeError(w, r, errs.ErrNotFound, map[string]string{"id": id})
		return
	}
	s.log.InfoContext(r.Context(), "oauth: grant revoked from settings", "grant", id, "owner", p.ActorID)
	w.WriteHeader(http.StatusNoContent)
}
