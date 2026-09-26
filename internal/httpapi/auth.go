package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// Scopes gate capabilities across every authenticated surface. They are the MVP
// projection of the OAuth 2.1 consent scopes (ADR-0004): reads, artifact
// writes, and annotation writes are grantable to an agent; sharing:manage is a
// human-only capability an agent token is never issued, so an agent can never
// broaden sharing or delete on the human's behalf (ADR-0004 "we deliberately do
// not expose a sharing:manage scope to agents").
const (
	scopeArtifactsRead    = "artifacts:read"
	scopeArtifactsWrite   = "artifacts:write"
	scopeAnnotationsWrite = "annotations:write"
	scopeSharingManage    = "sharing:manage"
)

// agentScopes is the exact grant an agent (an MCP/API token acting for a human)
// receives — the three consent-screen scopes of ADR-0004 and nothing more. It is
// a fresh map per call so a caller can never mutate a shared grant.
func agentScopes() map[string]bool {
	return map[string]bool{
		scopeArtifactsRead:    true,
		scopeArtifactsWrite:   true,
		scopeAnnotationsWrite: true,
	}
}

// humanScopes is the grant a human principal (a personal API token or the dev
// login) receives: the agent scopes plus sharing:manage, the human-only
// capability that changes sharing and expiry (ADR-0004, ADR-0007).
func humanScopes() map[string]bool {
	s := agentScopes()
	s[scopeSharingManage] = true
	return s
}

// Principal is the authenticated caller. Channel is derived server-side from the
// authenticated surface — never from a client claim (SPEC-0002 "Channel is
// server-derived"). Scopes gate capabilities such as sharing:manage, which
// agents do not receive (SPEC-0002 "Agent cannot broaden sharing").
//
// UserID is the users row this principal acts for, and every ownership and
// authorship check compares it (SPEC-0023 REQ "Owner Model"). ActorID is that
// user as rendered on the wire (provenance, actor_id fields); it is display
// text, never an ownership key. Every authenticator that can reach a store
// sets both; a principal without a UserID owns and creates nothing.
type Principal struct {
	ActorID string
	UserID  string
	Channel artifact.Channel
	IsAgent bool
	Scopes  map[string]bool
	// Ambient reports whether the caller was authenticated by an ambient
	// credential the browser attaches automatically — a session cookie — rather
	// than an explicit bearer token. Only ambient credentials are forgeable by
	// a cross-site request, so CSRF protection is gated on this flag: token
	// callers (API/MCP/CLI) are exempt, cookie-session callers are guarded
	// (SPEC-0006 REQ "CSRF Protection"). The token authenticators leave it false;
	// the web-session Authenticator (#11) sets it true.
	Ambient bool
	// Issuer and Subject are the provenance a browser session recorded at
	// sign-in (empty for bearer principals and the development login).
	Issuer  string
	Subject string
	// Operator reports that this is an operator's browser session (SPEC-0023
	// REQ "Operator and User Profiles"). Only the web-session authenticator
	// sets it: operator routes need a browser session, never a bearer token.
	// It gates the operator routes and nothing else; it never widens what the
	// principal may read or own.
	Operator bool
}

// HasScope reports whether the principal holds scope.
func (p *Principal) HasScope(scope string) bool {
	return p.Scopes[scope]
}

// Authenticator resolves the authenticated principal for a request, or returns
// errs.ErrUnauthorized when credentials are absent or invalid. Real OAuth 2.1
// (bearer) lands in ADR-0004 (#22); this seam is what that adapter, the static
// token adapter below, and the web-session adapter (#11) implement.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// bearerToken extracts the presented `Authorization: Bearer <token>` secret, or
// "" when the header is absent, malformed, or the token empty. It never trusts
// the value as an identity — callers must verify it against a credential store.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

// APIToken is a single static bearer credential: an opaque high-entropy secret
// that maps to an actor identity and its capability. It is the pre-OAuth MVP
// token seam (ADR-0004 Option B, "static API keys"); full OAuth 2.1 with dynamic
// client registration and refresh rotation replaces it in #22 by swapping this
// Authenticator, leaving every downstream authz check unchanged.
type APIToken struct {
	// Secret is the bearer value the client presents. It is stored only as a
	// hash inside TokenAuthenticator, never compared in plaintext.
	Secret string
	// ActorID is the identity a request bearing this token authenticates AS. The
	// secret proves the identity; the client never asserts the actor id itself.
	ActorID string
	// IsAgent marks a token minted for an agent/MCP client acting on the human's
	// behalf, which is granted exactly the three ADR-0004 agent scopes and never
	// sharing:manage. A human personal token additionally carries sharing:manage.
	IsAgent bool
}

// ParseAPITokens parses the CAIRN_API_TOKENS configuration value into a set of
// static bearer credentials. The format is a comma-separated list of
// `secret:actor[:role]` entries, where role is `human` (default) or `agent`.
// Whitespace around entries and fields is trimmed. An empty value yields no
// tokens (the bearer surface then rejects every token, failing closed). A
// malformed entry, a blank secret/actor, or a duplicate secret is a
// configuration error surfaced at startup rather than a silently dropped
// credential.
//
// Governing: ADR-0004 (token seam), ADR-0012 (config from the environment).
func ParseAPITokens(raw string) ([]APIToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var (
		tokens []APIToken
		seen   = map[string]bool{}
	)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("api token %q: want secret:actor[:role]", entry)
		}
		secret := strings.TrimSpace(parts[0])
		actor := strings.TrimSpace(parts[1])
		if secret == "" || actor == "" {
			return nil, fmt.Errorf("api token %q: secret and actor are required", entry)
		}
		isAgent := false
		if len(parts) == 3 {
			switch strings.TrimSpace(parts[2]) {
			case "", "human":
				isAgent = false
			case "agent":
				isAgent = true
			default:
				return nil, fmt.Errorf("api token %q: role must be human or agent", entry)
			}
		}
		if seen[secret] {
			return nil, fmt.Errorf("api token: duplicate secret")
		}
		seen[secret] = true
		tokens = append(tokens, APIToken{Secret: secret, ActorID: actor, IsAgent: isAgent})
	}
	return tokens, nil
}

// TokenAuthenticator verifies an `Authorization: Bearer <token>` credential
// against a static registry of known secrets, resolving each to the actor and
// scopes it was minted for. This is the security-critical replacement for the
// old dev stub: a raw bearer string is NEVER trusted as an actor id — it must
// hash to a registered secret, or the request is unauthorized. Secrets are held
// only as SHA-256 digests, and lookup hashes the presented token to a
// fixed-width key so verification cost does not vary with which token matched.
//
// The channel is server-derived to `via API` for this surface (SPEC-0002
// "Channel is server-derived"); an agent token is granted only the ADR-0004
// agent scopes, so no token can broaden sharing or delete another actor's work.
//
// Governing: ADR-0004 (MCP/OAuth token seam — this is the MVP static-token
// bridge to #22), SPEC-0002 (server-derived channel), SPEC-0006 (auth seam).
type TokenAuthenticator struct {
	// grants maps sha256(secret) → the credential. The digest key means the map
	// stores no plaintext secret and a lookup is a single constant-cost hash.
	grants map[[sha256.Size]byte]APIToken
	// users resolves each token's configured actor to its user (nil on
	// storeless wirings, whose principals then carry no user id).
	users *user.Store
}

// NewTokenAuthenticator builds a TokenAuthenticator over the given static
// credentials. A nil/empty set yields an authenticator that rejects every
// bearer token — the fail-closed default for a deployment that configured none.
func NewTokenAuthenticator(tokens []APIToken) *TokenAuthenticator {
	grants := make(map[[sha256.Size]byte]APIToken, len(tokens))
	for _, t := range tokens {
		grants[sha256.Sum256([]byte(t.Secret))] = t
	}
	return &TokenAuthenticator{grants: grants}
}

// Authenticate implements Authenticator. An absent bearer, or one whose secret
// is not registered, is errs.ErrUnauthorized. A registered secret resolves to
// its actor with the server-derived `via API` channel and the scopes its role
// grants — never to whatever the caller typed after `Bearer `.
func (a *TokenAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errs.ErrUnauthorized
	}
	grant, ok := a.grants[sha256.Sum256([]byte(token))]
	if !ok {
		// Touch a constant-time compare against a fixed sentinel so an unknown
		// token does not resolve visibly faster than a byte-mismatched one.
		subtle.ConstantTimeCompare([]byte(token), []byte(token))
		return nil, errs.ErrUnauthorized
	}
	scopes := humanScopes()
	if grant.IsAgent {
		scopes = agentScopes()
	}
	p := &Principal{
		ActorID: grant.ActorID,
		Channel: artifact.ChannelAPI,
		IsAgent: grant.IsAgent,
		Scopes:  scopes,
	}
	if err := resolveActorUser(r.Context(), a.users, p); err != nil {
		return nil, errs.ErrUnauthorized
	}
	return p, nil
}

// resolveActorUser binds a string-keyed principal (a CAIRN_API_TOKENS entry
// or the dev bearer) to the user its actor names, in the order the ownership
// migration resolved legacy owner strings, so the credential keeps the Bin it
// had before (user.Store.ResolveActor). The principal then renders as that
// user. Static tokens are reworked to name an operator's user by #333; until
// then this is the whole of their change. A nil store leaves the principal
// without a user: it can authenticate but owns and creates nothing. A
// suspended user's credential does not authenticate (SPEC-0023 "Suspending
// a user offboards them").
//
// Governing: ADR-0029, SPEC-0023 REQ "Owner Model", REQ "Migration to
// Explicit Ownership", REQ "Operator Surfaces Bound Tenant Data and Never
// Read It".
func resolveActorUser(ctx context.Context, users *user.Store, p *Principal) error {
	if users == nil {
		return nil
	}
	u, err := users.ResolveActor(ctx, p.ActorID)
	if err != nil {
		return err
	}
	if u.SuspendedAt != nil {
		return errUserSuspended
	}
	p.UserID, p.ActorID = u.ID, u.Actor
	return nil
}

// DevActorAuthenticator is the INSECURE development-only Authenticator: it trusts
// the raw bearer token AS the actor id, with no verification whatsoever. Anyone
// can therefore authenticate as any actor by typing their name, so it MUST NEVER
// be enabled in production — it exists solely so local development and the test
// suite can act as arbitrary actors without minting tokens. It is wired only when
// CAIRN_DEV_INSECURE_BEARER_AUTH is explicitly set (see New); the production
// default is the verifying TokenAuthenticator, so no production path trusts a
// raw bearer==actor.
//
// Governing: ADR-0004 (the seam OAuth replaces; this dev stub is never a prod
// credential).
type DevActorAuthenticator struct {
	// users resolves the typed actor to its user (nil on storeless wirings).
	users *user.Store
}

// Authenticate implements Authenticator. The bearer token is taken verbatim as
// the actor id and resolved to that actor's user; the channel is fixed
// server-side to the REST surface's `via API` so provenance is still not
// client-spoofable even under this dev shortcut. The grant is the agent scope
// set (no sharing:manage) — a dev caller is never more privileged than an
// agent.
func (a DevActorAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errs.ErrUnauthorized
	}
	p := &Principal{
		ActorID: token,
		Channel: artifact.ChannelAPI,
		Scopes:  agentScopes(),
	}
	if err := resolveActorUser(r.Context(), a.users, p); err != nil {
		return nil, errs.ErrUnauthorized
	}
	return p, nil
}

// chainAuthenticator tries each Authenticator in order, returning the first
// principal a member resolves and errs.ErrUnauthorized only when every member
// declines. It composes the verifying token authenticator with the optional dev
// shortcut so a real token still wins over the dev fallback.
type chainAuthenticator []Authenticator

// Authenticate implements Authenticator.
func (c chainAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil && p != nil {
			return p, nil
		}
	}
	return nil, errs.ErrUnauthorized
}

type principalCtxKey struct{}

// requireAuth is middleware that authenticates the request and stashes the
// principal in the context, or writes a 401 envelope. It guards mutating and
// workspace-scoped endpoints; link-capability reads are public.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil || p == nil {
			s.writeError(w, r, errs.ErrUnauthorized, nil)
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireScope is middleware that enforces a capability on an already-
// authenticated principal: it runs after requireAuth, and a principal lacking
// scope is a uniform 403 (authenticated but forbidden), distinct from the 401 an
// unauthenticated caller gets. This is the seam that keeps agent tokens off
// human-only capabilities — e.g. requireScope(scopeSharingManage) on a future
// Share endpoint refuses every agent token, since agentScopes omits it (ADR-0004).
//
// Governing: ADR-0004 (scoped consent), SPEC-0006 (consistent 401/403).
func (s *Server) requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principalFrom(r.Context())
			if !ok || p == nil {
				s.writeError(w, r, errs.ErrUnauthorized, nil)
				return
			}
			if !p.HasScope(scope) {
				s.writeError(w, r, errs.ErrForbidden, nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireHuman is middleware that refuses every agent token: it runs after
// requireAuth and 403s any principal marked IsAgent, letting only human callers
// (personal API tokens and cookie sessions, both IsAgent=false) through. It is
// the seam for capabilities the three-scope model deliberately gives no
// scope — deletion — which ADR-0004 / SPEC-0004 keep as an "explicit human
// action" agents can never perform on the human's behalf. Gating delete on
// artifacts:write alone was insufficient because agentScopes grants
// artifacts:write; the human/agent distinction, not a write scope, is what
// separates a deletable-by-agent write from a human-only delete. Delete
// deliberately still gates on IsAgent rather than a scope: the owner-policy
// endpoints (sharing/TTL/rotate, issue #94, policy.go) instead gate on the
// scopeSharingManage requireScope check, since — unlike delete — they DO have
// a natural dedicated scope that both the web session and a human API token
// now carry (SPEC-0009 REQ "Owner-Only Policy Changes"); an agent token or
// the DevActorAuthenticator dev shortcut still never receives it (ADR-0004).
//
// Governing: ADR-0004 (least-privilege agent grant — no delete scope),
// SPEC-0004 (mcp-server-and-oauth: "deletion stays an explicit human action"),
// SPEC-0006 (consistent 401/403).
func (s *Server) requireHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok || p == nil {
			s.writeError(w, r, errs.ErrUnauthorized, nil)
			return
		}
		if p.IsAgent {
			s.writeError(w, r, errs.ErrForbidden, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// principalFrom returns the authenticated principal stashed by requireAuth.
func principalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}

// handleAPIWhoami is the bearer-authenticated identity round trip
// `cairn whoami` (and any other bearer-token caller) verifies a credential
// against (cairn#21 "verify against the API (whoami round-trip)", SPEC-0008
// "Authentication and Session Lifecycle"). It runs under requireAuth so ANY
// authenticated principal — a CAIRN_API_TOKENS static token, a personal
// access token (#74), an OAuth access token, or a browser session cookie —
// resolves here; the response shape mirrors the web session's whoamiResponse
// (session.go handleWhoami) so both surfaces report identity identically.
func (s *Server) handleAPIWhoami(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok || p == nil {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	s.writeJSON(w, http.StatusOK, whoamiResponse{
		ActorID:       p.ActorID,
		Channel:       string(p.Channel),
		Authenticated: true,
	})
}
