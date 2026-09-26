package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/session"
)

// The minimal web session surface (SPEC-0001, ADR-0004). A browser logs in once
// against the dev credential verifier, receives a secure httpOnly session cookie
// plus the readable double-submit CSRF cookie, and is thereafter an *ambient*
// principal: authenticated by a cookie the browser attaches automatically, so
// its state-changing requests are CSRF-guarded (see enforceCSRF), while its
// reads follow the same link capability as any caller. whoami and logout close
// the loop. This whole surface is the seam OAuth 2.1 (#22) replaces: it swaps
// the credential verifier and the Authenticator adapter, keeping the session
// store, cookies, CSRF, and provenance wiring unchanged.
//
// Governing: SPEC-0001 (Authentication & Authorization, CSRF Protection,
// Redirect & SSRF Validation), SPEC-0009 (server-derived actor/channel),
// ADR-0004 (auth seam), ADR-0007 (link-capability reads stay public).

// sessionCookieName is the opaque session token cookie. It is httpOnly (no
// script can read it), SameSite=Lax, and Secure over HTTPS — the credential a
// cross-site request cannot forge on its own, which is exactly why cookie
// (ambient) principals are CSRF-guarded and token principals are not.
const sessionCookieName = "cairn_session"

// CredentialVerifier resolves a login form's (actor, secret) into the actor id a
// session is minted for, or false when the credential is rejected. It is the
// swap-in seam: the dev password verifier below is replaced by OAuth / an
// upstream IdP (#22) without touching session issuance. Governing: ADR-0004.
type CredentialVerifier interface {
	Verify(actorID, secret string) bool
}

// DevPasswordVerifier is the MVP login stub: any actor id authenticates with the
// single shared dev password. An empty password disables login entirely (the
// deployment opted out), so a misconfigured instance fails closed rather than
// accepting a blank secret.
type DevPasswordVerifier struct {
	Password string
}

// Verify reports whether the presented secret matches the configured dev
// password under a constant-time compare. Login is refused outright when no
// password is configured or the actor id is blank.
func (v DevPasswordVerifier) Verify(actorID, secret string) bool {
	if v.Password == "" || actorID == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(v.Password)) == 1
}

// SessionAuthenticator is the web-aware Authenticator adapter. It resolves a
// request to a principal by first honoring an explicit bearer token (API / MCP /
// CLI, non-ambient), then falling back to the session cookie (web, ambient). A
// bearer caller therefore keeps the API's non-ambient, CSRF-exempt semantics
// even on the web binary, while a browser session is ambient and CSRF-guarded.
// The channel is server-derived per surface (SPEC-0009): via API for the token
// path, via web for the cookie path — never a client claim.
//
// Governing: ADR-0004 (one Authenticator seam), SPEC-0009 (server-derived
// actor/channel), SPEC-0006 (CSRF gated on Ambient).
type SessionAuthenticator struct {
	sessions session.Store
	bearer   Authenticator
}

// Authenticate implements Authenticator. Order matters: an explicit bearer token
// wins so a scripted caller is never accidentally treated as an ambient browser.
func (a *SessionAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	if a.bearer != nil {
		if p, err := a.bearer.Authenticate(r); err == nil {
			return p, nil
		}
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errs.ErrUnauthorized
	}
	sess, err := a.sessions.Get(r.Context(), cookie.Value)
	if err != nil {
		return nil, errs.ErrUnauthorized
	}
	// The session actor is the human this browser acts as; the channel is the web
	// surface, derived here and never taken from the request. The session is
	// Ambient, so every state-changing write it makes — including the owner
	// policy endpoints below — is CSRF-guarded. A logged-in human may create
	// artifacts and comment/react from the shell (artifacts:write,
	// annotations:write) and, as of issue #94 (the Share dialog's owner
	// controls), also change sharing/TTL and rotate the id from the same
	// session — sharing:manage is still a human-only scope (ADR-0004 / SPEC-0007
	// "sharing:manage is human-only"; no agent token or DevActorAuthenticator
	// principal ever carries it), it is simply no longer withheld from the web
	// session now that the UI that exercises it exists.
	return &Principal{
		ActorID: sess.ActorID,
		Channel: artifact.ChannelWeb,
		Scopes:  map[string]bool{scopeArtifactsWrite: true, scopeAnnotationsWrite: true, scopeSharingManage: true},
		Ambient: true,
	}, nil
}

// loginView is the login page view model: the per-request CSRF token (embedded
// as a hidden field and mirrored in the readable cookie), the validated
// post-login destination, whether the dev-password form is available, whether
// OIDC ("Sign in with Pocket ID") is available, and whether the prior
// dev-password attempt failed (a generic, field-agnostic failure).
type loginView struct {
	CSRFToken    string
	Next         string
	LoginEnabled bool
	OIDCEnabled  bool
	// GitHubLogin gates the "Log in with GitHub" button (SPEC-0012).
	GitHubLogin bool
	Failed      bool
}

// handleLoginForm renders the login page. A per-request CSRF token is minted into
// a readable cookie and embedded as a hidden field so the no-JS form post can be
// double-submit validated — login itself must resist cross-site submission even
// before a session exists. The optional ?next carries the post-login
// destination, validated against internal paths only at redirect time.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if p, err := s.auth.Authenticate(r); err == nil && p != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	token, err := session.NewToken()
	if err != nil {
		s.log.ErrorContext(r.Context(), "web: mint login csrf failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, csrfCookieName, token, s.cfg.SessionTTL, false)
	next := safeNext(r.URL.Query().Get("next"))
	s.renderWeb(w, r, "login", loginView{
		CSRFToken:    token,
		Next:         next,
		LoginEnabled: s.loginEnabled(),
		OIDCEnabled:  s.oidc != nil,
		GitHubLogin:  s.gh != nil,
		Failed:       r.URL.Query().Get("error") == "1",
	})
}

// handleLogin verifies the posted credential, mints a server-side session, and
// sets the session + CSRF cookies. The form is double-submit CSRF checked
// (hidden field vs the readable cookie) so a foreign origin cannot force a login.
// The post-login redirect target is validated against internal paths, so a
// crafted ?next=https://evil is ignored (SPEC-0001 Redirect & SSRF Validation).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginEnabled() {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationRequestBytes)
	if err := r.ParseForm(); err != nil {
		s.renderWebError(w, r, errs.Validationf("login: malformed form"))
		return
	}
	if !validFormCSRF(r) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	actor := strings.TrimSpace(r.PostFormValue("actor"))
	secret := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	if !s.verifier.Verify(actor, secret) {
		// Re-render the form with a generic failure; never say which field was
		// wrong so the surface leaks nothing about valid actor ids.
		http.Redirect(w, r, "/login?error=1&next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	// Dev-password sessions carry no provider provenance: empty issuer and
	// subject (SPEC-0012 records provenance only for real IdP logins).
	sess, err := s.sessions.Create(r.Context(), "", "", actor, s.cfg.SessionTTL)
	if err != nil {
		s.log.ErrorContext(r.Context(), "web: create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// httpOnly session cookie (unforgeable by script) + readable CSRF cookie
	// bound to this session (the double-submit secret the shell echoes).
	s.setCookie(w, r, sessionCookieName, sess.Token, s.cfg.SessionTTL, true)
	s.setCookie(w, r, csrfCookieName, sess.CSRFToken, s.cfg.SessionTTL, false)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout revokes the current session and clears both cookies. It is a POST
// double-submit CSRF checked so a foreign origin cannot force a logout, then
// redirects to the login page. Logout is idempotent — no session is not an error.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnotationRequestBytes)
	_ = r.ParseForm()
	if !validFormCSRF(r) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if err := s.sessions.Delete(r.Context(), c.Value); err != nil {
			s.log.WarnContext(r.Context(), "web: delete session failed", "error", err)
		}
	}
	s.clearCookie(w, r, sessionCookieName)
	s.clearCookie(w, r, csrfCookieName)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// whoamiResponse is the JSON identity view the whoami endpoint returns for the
// current session — the parity of the CLI `whoami` over the web surface.
type whoamiResponse struct {
	ActorID       string `json:"actor_id"`
	Channel       string `json:"channel"`
	Authenticated bool   `json:"authenticated"`
}

// handleWhoami returns the current session's actor and server-derived channel. It
// runs under requireWebSession, so an unauthenticated caller is redirected to
// login rather than reaching here.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	s.writeJSON(w, http.StatusOK, whoamiResponse{
		ActorID:       p.ActorID,
		Channel:       string(p.Channel),
		Authenticated: true,
	})
}

// requireWebSession guards workspace-scoped web routes (the Bin, whoami). Unlike
// the API's requireAuth (which writes a JSON 401), an unauthenticated browser is
// redirected to the login page carrying a validated ?next so it returns to the
// page it wanted after signing in (SPEC-0001 Authentication & Authorization).
func (s *Server) requireWebSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil || p == nil {
			http.Redirect(w, r, s.loginRedirectPath()+"?next="+url.QueryEscape(safeNext(r.URL.RequestURI())), http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loginRedirectPath is where an unauthenticated web route sends the browser.
// When OIDC is configured it goes straight into the passwordless flow at
// /auth/login — no intermediate form to click through, matching "passwordless,
// one identity, no second login" (issue #55, ADR-0013). Otherwise it goes to
// the interactive /login form (the dev-only password fallback).
func (s *Server) loginRedirectPath() string {
	if s.oidc != nil {
		return "/auth/login"
	}
	return "/login"
}

// loginEnabled reports whether the dev-password form can be used to establish
// a session. Demoted by ADR-0013 to a local-dev-only fallback: it additionally
// requires OIDC to be unconfigured, so a deployment that has wired Pocket ID
// can never fall back to the shared dev password even if one is still set in
// its environment. A session store, a credential verifier, and a configured
// dev password must all be present too (a pure-unit server wiring, or a
// deployment that set no password, has login disabled and fails closed).
func (s *Server) loginEnabled() bool {
	return s.oidc == nil && s.sessions != nil && s.verifier != nil && s.cfg.DevLoginPassword != ""
}

// setCookie writes a cookie scoped to the whole site. httpOnly is set for the
// session token (script must never read it) and cleared for the CSRF token (a
// same-origin script must read it to echo it in the request header). Secure
// tracks the public origin's scheme so the cookie is TLS-only in production even
// behind a TLS-terminating proxy where r.TLS is nil.
func (s *Server) setCookie(w http.ResponseWriter, _ *http.Request, name, value string, ttl time.Duration, httpOnly bool) {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: httpOnly,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie expires a cookie by name (MaxAge<0), matching the attributes used
// to set it so the browser drops it.
func (s *Server) clearCookie(w http.ResponseWriter, _ *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: name == sessionCookieName,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// validFormCSRF is the no-JS double-submit check for the login and logout forms:
// the hidden csrf_token field must match the readable cairn_csrf cookie under a
// constant-time compare. It complements enforceCSRF (which checks the header for
// script-driven posts); a form post carries no header, so it proves origin with
// the field instead.
func validFormCSRF(r *http.Request) bool {
	field := r.PostFormValue("csrf_token")
	if field == "" {
		return false
	}
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(field), []byte(cookie.Value)) == 1
}

// safeNext validates a post-login/redirect target against internal paths only: a
// value is honored solely when it is a single-slash-rooted relative path (not a
// protocol-relative "//host" or absolute URL), else the Bin is substituted. No
// user-supplied absolute URL is ever a redirect target (SPEC-0001 Redirect &
// SSRF Validation — open-redirect defense).
func safeNext(next string) string {
	// Reject empty, non-rooted, protocol-relative ("//host"), and backslash-
	// smuggled ("/\host", which several browsers normalize to "//host") targets.
	if next == "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/bin"
	}
	// Reject anything that parses to an absolute URL or carries a host — a bare
	// internal path has neither scheme nor host.
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/bin"
	}
	return u.RequestURI()
}
