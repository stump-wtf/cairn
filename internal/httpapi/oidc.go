// Cairn is a self-contained OIDC relying party: it logs the human in directly
// against Pocket ID (authorization-code + PKCE + nonce), rather than sitting
// behind an oauth2-proxy forward-auth layer. The flow mints the SAME server-side
// session the dev-password login does (session.go), so every downstream
// consumer — the Bin, the comment composer, and critically the /oauth/authorize
// consent screen (ADR-0004) — needs no OIDC-awareness of its own: an
// OIDC-authenticated session IS a web session, full stop.
//
// This mirrors ~/src/switchboard's internal/auth OIDC relying party (same
// libraries: github.com/coreos/go-oidc/v3 + golang.org/x/oauth2, same
// state+nonce+PKCE short-lived cookie shape), adapted to Cairn's existing
// session store (session.Store, keyed on a bare actor id string — Cairn has no
// separate "human" table to upsert into) and its existing cookie/CSRF helpers.
//
// Governing: ADR-0013 (native OIDC relying party), issue #55.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/joestump/cairn/internal/session"
)

const (
	// oidcStateCookieName carries the per-login state, nonce, PKCE verifier, and
	// validated post-login destination across the redirect to Pocket ID and back.
	// It is short-lived and cleared as soon as the callback consumes it.
	oidcStateCookieName = "cairn_oidc_state"
	oidcStateTTL        = 10 * time.Minute
)

// oidcRP is the discovered OIDC relying-party wiring: the provider (issuer
// metadata + JWKS, fetched once at EnableOIDC), the oauth2 client config, and
// the ID-token verifier. A nil *oidcRP on Server means OIDC is not configured —
// the swap-in seam session.go's CredentialVerifier comment anticipated.
type oidcRP struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// EnableOIDC discovers the configured issuer and wires the OIDC relying-party
// login flow. It is a no-op (returns nil, leaves s.oidc nil) when
// CAIRN_OIDC_ISSUER is unset — the local dev_login_password fallback then
// stays the only login path (ADR-0013). Call once at startup; a discovery
// failure is returned so the caller can fail fast rather than silently run
// with human login broken.
//
// Governing: ADR-0013, issue #55.
func (s *Server) EnableOIDC(ctx context.Context) error {
	if s.cfg.OIDCIssuer == "" {
		return nil
	}
	provider, err := oidc.NewProvider(ctx, s.cfg.OIDCIssuer)
	if err != nil {
		return fmt.Errorf("oidc: discover issuer %s: %w", s.cfg.OIDCIssuer, err)
	}
	clientID := firstNonEmpty(s.cfg.OIDCClientID, "cairn")
	s.oidc = &oidcRP{
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: s.cfg.OIDCClientSecret,
			RedirectURL:  s.cfg.BaseURL + "/auth/callback",
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}
	return nil
}

// oidcState is the short-lived per-login state stashed in oidcStateCookieName
// across the redirect to Pocket ID and back: the CSRF-resistant state value,
// the replay-resistant nonce bound into the ID token, the PKCE code verifier,
// and the validated post-login destination (so OIDC login honors the same
// ?next= the dev-password form does).
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
}

// handleOIDCLogin begins the OIDC authorization-code flow (GET /auth/login):
// mint state + nonce + a PKCE verifier, stash them (plus the validated ?next)
// in a short-lived cookie, and redirect to Pocket ID's authorization endpoint.
// A caller reaching this route with OIDC unconfigured gets a clear 503 — the
// route is only linked from the login page when s.oidc != nil, so this is a
// defensive floor, not the primary guard.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "OIDC is not configured on this instance", http.StatusServiceUnavailable)
		return
	}
	state, err := session.NewToken()
	if err != nil {
		s.log.ErrorContext(r.Context(), "oidc: generate state failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := session.NewToken()
	if err != nil {
		s.log.ErrorContext(r.Context(), "oidc: generate nonce failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st := oidcState{
		State:    state,
		Nonce:    nonce,
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		s.log.ErrorContext(r.Context(), "oidc: marshal state failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, oidcStateCookieName, base64.RawURLEncoding.EncodeToString(raw), oidcStateTTL, true)
	authURL := s.oidc.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback completes the flow (GET /auth/callback): validates the
// state cookie against the callback's state parameter, exchanges the code
// (presenting the stashed PKCE verifier), verifies the ID token against the
// issuer's JWKS (signature, issuer, audience, expiry — go-oidc's default
// checks), confirms the nonce round-trips, then establishes Cairn's ordinary
// web session with the human's email (falling back to the OIDC subject when no
// email claim is present) as the actor. Every rejection path establishes no
// session and leaves no trace of a partial login.
//
// Governing: ADR-0013, issue #55 checklist "state mismatch rejected",
// "expired/invalid ID token rejected".
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.Error(w, "OIDC is not configured on this instance", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "missing state cookie")
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "malformed state cookie")
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" || st.State != r.URL.Query().Get("state") {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "state mismatch")
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	// Clear the state cookie immediately: it is single-use whether the rest of
	// the callback succeeds or fails.
	s.clearCookie(w, r, oidcStateCookieName)

	token, err := s.oidc.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.log.WarnContext(r.Context(), "oidc token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "no id_token in token response")
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	// Verify checks issuer, audience, signature (against the issuer's fetched
	// JWKS), and expiry — an expired or badly-signed token fails here.
	idToken, err := s.oidc.verifier.Verify(r.Context(), rawID)
	if err != nil {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "id_token verification failed", "error", err)
		http.Error(w, "id_token verify failed", http.StatusBadGateway)
		return
	}
	if idToken.Nonce != st.Nonce {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "nonce mismatch")
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims struct {
		Email string `json:"email"`
	}
	_ = idToken.Claims(&claims)
	actor := firstNonEmpty(claims.Email, idToken.Subject)
	if actor == "" {
		s.log.ErrorContext(r.Context(), "oidc callback rejected", "reason", "empty subject")
		http.Error(w, "id_token has no usable subject", http.StatusBadGateway)
		return
	}

	sess, err := s.sessions.Create(r.Context(), actor, s.cfg.SessionTTL)
	if err != nil {
		s.log.ErrorContext(r.Context(), "oidc: create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, sessionCookieName, sess.Token, s.cfg.SessionTTL, true)
	s.setCookie(w, r, csrfCookieName, sess.CSRFToken, s.cfg.SessionTTL, false)
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}
