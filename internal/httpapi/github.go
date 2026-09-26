// GitHub login dispatch (SPEC-0012): the /auth/login and /auth/callback
// routes previously went straight to the OIDC handlers; they are now a
// two-provider dispatcher selected by the ?provider= parameter. The selected
// provider id travels inside the SHARED state cookie (oidcState.Provider), so
// the callback needs no second cookie, no second TTL, and no second
// single-use discipline — and a cookie written before this change carries no
// Provider at all, which resolves to the Pocket ID path exactly as before
// (the "byte-for-byte unchanged when provider is absent" requirement).
//
// Governing: SPEC-0012, ADR-0017.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/stump-wtf/cairn/internal/httpapi/authprovider"
	"github.com/stump-wtf/cairn/internal/session"
)

// EnableGitHub wires the GitHub login provider when configured. A no-op
// (leaves s.gh nil) when CAIRN_GITHUB_CLIENT_ID/SECRET are unset — the
// provider is then absent from the registry and the routes 404 for
// ?provider=github. Call once at startup.
func (s *Server) EnableGitHub() {
	if s.cfg.GitHubClientID == "" || s.cfg.GitHubClientSecret == "" {
		return
	}
	s.gh = authprovider.NewGitHubProvider(s.cfg.GitHubClientID, s.cfg.GitHubClientSecret, s.cfg.BaseURL)
}

// providerFor resolves the provider registry entry for a ?provider= value.
// An empty name selects the legacy default (Pocket ID via the OIDC handlers,
// ok=true with p=nil). Unknown or unconfigured names return ok=false — the
// caller 404s, making an unconfigured provider indistinguishable from an
// unknown one.
func (s *Server) providerFor(name string) (p authprovider.Provider, ok bool) {
	switch name {
	case "":
		// The legacy default routes to the OIDC handlers even when OIDC is
		// unconfigured, so their own 503 surface ("OIDC is not configured")
		// is preserved byte-for-byte — a 404 here would change pre-0012
		// behavior. GitHub, which has no legacy surface, 404s instead.
		return nil, true
	case "github":
		return s.gh, s.gh != nil
	default:
		return nil, false
	}
}

// handleLoginStart is GET /auth/login. Dispatches on ?provider= (empty =
// Pocket ID, the legacy default) and never falls through to a provider that
// is not configured: the 404 here is the acceptance criterion for an
// unconfigured provider.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFor(r.URL.Query().Get("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if p == nil {
		// Pocket ID keeps its PKCE+nonce OIDC start untouched.
		s.handleOIDCLogin(w, r)
		return
	}
	s.handleGitHubLogin(w, r)
}

// handleLoginCallback is GET /auth/callback. The state cookie — not a query
// parameter — says which provider began this login, so a callback can only
// ever complete a flow that a cookie on THIS browser started; a provider
// query parameter that contradicts the cookie is rejected as a mismatch.
func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil {
		s.log.WarnContext(r.Context(), "login callback rejected", "reason", "missing state cookie")
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "login callback rejected", "reason", "malformed state cookie")
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil {
		s.log.WarnContext(r.Context(), "login callback rejected", "reason", "unparseable state cookie")
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	// A callback ?provider= that contradicts the cookie's provider is an
	// attempt to swap flows mid-handshake: reject before anything is
	// exchanged. Absent → the cookie decides.
	if q := r.URL.Query().Get("provider"); q != "" && q != st.Provider {
		s.log.WarnContext(r.Context(), "login callback rejected", "reason", "provider mismatch")
		http.Error(w, "provider mismatch", http.StatusBadRequest)
		return
	}
	switch st.Provider {
	case "":
		s.handleOIDCCallback(w, r)
	case "github":
		s.handleGitHubCallback(w, r)
	default:
		s.log.WarnContext(r.Context(), "login callback rejected", "reason", "unknown provider in state cookie")
		http.NotFound(w, r)
	}
}

// handleGitHubLogin begins the GitHub flow: mint state, stash it (with
// Provider=github and the validated ?next) in the shared state cookie, and
// redirect to GitHub's authorization endpoint. The route 404s when GitHub is
// unconfigured — same indistinguishability rule as the dispatcher.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.gh == nil {
		http.NotFound(w, r)
		return
	}
	state, err := session.NewToken()
	if err != nil {
		s.log.ErrorContext(r.Context(), "github: generate state failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	st := oidcState{State: state, Provider: "github", Next: next}
	raw, err := json.Marshal(st)
	if err != nil {
		s.log.ErrorContext(r.Context(), "github: marshal state failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, oidcStateCookieName, base64.RawURLEncoding.EncodeToString(raw), oidcStateTTL, true)
	redirect, err := s.gh.StartLogin(state, next)
	if err != nil {
		s.log.ErrorContext(r.Context(), "github: start login failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// handleGitHubCallback completes the GitHub flow against the stashed state.
// The token exchange and profile fetch happen inside the provider; this
// handler owns the state cookie discipline (validate, clear immediately,
// single-use) and the session establishment, identical in shape to the OIDC
// callback's. The provider's access token never reaches this layer at all.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.gh == nil {
		http.NotFound(w, r)
		return
	}
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil {
		s.log.WarnContext(r.Context(), "github callback rejected", "reason", "missing state cookie")
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "github callback rejected", "reason", "malformed state cookie")
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil || st.Provider != "github" {
		s.log.WarnContext(r.Context(), "github callback rejected", "reason", "state cookie is not a github login")
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	// Clear the state cookie immediately: single-use whether the rest of the
	// callback succeeds or fails.
	s.clearCookie(w, r, oidcStateCookieName)

	identity, err := s.gh.FinishLogin(r.Context(), authprovider.State{
		State: st.State, Provider: st.Provider, Next: st.Next,
	}, r)
	if err != nil {
		// Reasons are logged (state mismatch, unverified email — never any
		// token); the surface gets a generic message.
		s.log.WarnContext(r.Context(), "github callback rejected", "reason", err.Error())
		http.Error(w, "github login failed", http.StatusBadGateway)
		return
	}
	if !authprovider.ValidateNext(st.Next) {
		s.log.WarnContext(r.Context(), "github callback rejected", "reason", "invalid next in state cookie")
		http.Error(w, "bad destination", http.StatusBadRequest)
		return
	}
	sess, err := s.sessions.Create(r.Context(), identity.Issuer, identity.Subject, identity.Actor, s.cfg.SessionTTL)
	if err != nil {
		s.log.ErrorContext(r.Context(), "github: create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, sessionCookieName, sess.Token, s.cfg.SessionTTL, true)
	s.setCookie(w, r, csrfCookieName, sess.CSRFToken, s.cfg.SessionTTL, false)
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}
