package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/session"
)

// The OAuth 2.1 authorization-server surface (SPEC-0007, ADR-0004): RFC 8414
// metadata discovery, RFC 7591 dynamic client registration, the
// authorization-code + PKCE grant with the human consent screen riding the
// existing web session, RFC 7009 revocation, and the bearer Authenticator that
// lets issued access tokens act on the /v1 (and future /mcp) surface. It is an
// in-process adapter over the oauth core service — the same layering the web
// and CLI surfaces use over the artifact core (ADR-0003, design.md
// "In-process adapter over the core").
//
// Governing: ADR-0004 (MCP OAuth 2.1 with scoped consent), SPEC-0007
// REQ "OAuth 2.1 Authorization-Code + PKCE", REQ "Metadata Discovery & Dynamic
// Client Registration", REQ "Exactly Three Consent Scopes", REQ "Token
// Revocation (RFC 7009)", REQ "Redirect & SSRF Validation".

// Body ceilings for the OAuth endpoints (SPEC-0007 REQ "Request Body Size
// Limits"): registration payloads and token/revoke/consent forms are tiny by
// construction, so anything larger is rejected with 413 before buffering.
const (
	maxRegisterRequestBytes = 64 << 10
	maxTokenRequestBytes    = 16 << 10
)

// oauthEnabled reports whether the authorization server is wired (a live store
// provides the Postgres it persists into; unit wirings leave it nil).
func (s *Server) oauthEnabled() bool { return s.oauth != nil }

// mountOAuthJSON registers the non-HTML authorization-server endpoints on the
// strict-CSP API group: discovery metadata, dynamic registration, token, and
// revocation. Discovery and the bootstrap endpoints are unavoidably public
// (they exist to make authorization possible) and carry the dedicated OAuth
// rate limiter on top of the shared per-IP limiter (SPEC-0007 REQ "Rate
// Limiting").
func (s *Server) mountOAuthJSON(r chi.Router) {
	if !s.oauthEnabled() {
		return
	}
	r.Get("/.well-known/oauth-authorization-server", s.handleOAuthASMetadata)
	r.Get("/.well-known/oauth-protected-resource", s.handleOAuthResourceMetadata)
	// RFC 9728 path-insertion form: an MCP client whose resource is
	// <base>/mcp looks for the metadata at /.well-known/oauth-protected-resource/mcp.
	r.Get("/.well-known/oauth-protected-resource/mcp", s.handleOAuthResourceMetadata)
	r.Group(func(r chi.Router) {
		r.Use(s.oauthRateLimit)
		r.Post("/oauth/register", s.handleOAuthRegister)
		r.Post("/oauth/token", s.handleOAuthToken)
		r.Post("/oauth/revoke", s.handleOAuthRevoke)
	})
}

// mountOAuthWeb registers the browser-facing authorization endpoint (login +
// consent) on the web group, under the web CSP and the OAuth rate limiter.
func (s *Server) mountOAuthWeb(r chi.Router) {
	if !s.oauthEnabled() {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(s.oauthRateLimit)
		r.Get("/oauth/authorize", s.handleAuthorizeForm)
		r.Post("/oauth/authorize", s.handleAuthorizeSubmit)
	})
}

// oauthRateLimit throttles the authorization-server endpoints per-IP with a
// tighter budget than the general limiter, blunting client-spraying on
// registration and code/refresh brute force on the token endpoint (SPEC-0007
// REQ "Rate Limiting"). Exceeding it is 429 with Retry-After.
func (s *Server) oauthRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.oauthLimiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		ok, retry := s.oauthLimiter.allow(clientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			s.writeOAuthError(w, r, http.StatusTooManyRequests, "slow_down", "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Discovery (RFC 8414 + protected-resource metadata) ---------------------

// asMetadata is the RFC 8414 authorization-server metadata document.
type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethods     []string `json:"revocation_endpoint_auth_methods_supported"`
}

// handleOAuthASMetadata serves RFC 8414 discovery so MCP clients find the
// endpoints without bespoke configuration. Public by necessity: metadata must
// be readable before a client can authenticate; it contains no secrets.
func (s *Server) handleOAuthASMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.BaseURL
	s.writeJSON(w, http.StatusOK, asMetadata{
		Issuer:                            base,
		AuthorizationEndpoint:             base + "/oauth/authorize",
		TokenEndpoint:                     base + "/oauth/token",
		RegistrationEndpoint:              base + "/oauth/register",
		RevocationEndpoint:                base + "/oauth/revoke",
		ScopesSupported:                   oauth.AllScopes(),
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		RevocationEndpointAuthMethods:     []string{"none"},
	})
}

// resourceMetadata is the protected-resource metadata document MCP clients use
// to locate the authorization server governing this resource.
type resourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// mcpResourceIndicator is the canonical RFC 8707 resource identifier for this
// deployment's MCP server: the /mcp endpoint URI. This is what MCP clients send
// as `resource` and what the protected-resource metadata advertises.
func (s *Server) mcpResourceIndicator() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/mcp"
}

// validResourceIndicator reports whether an RFC 8707 `resource` indicator names
// this deployment. An empty value is allowed (the indicator is optional). A
// present value must be our own origin or any path under it — this accepts both
// the MCP canonical URI (<base>/mcp) and the bare origin while rejecting any
// foreign audience, so a token minted here can never be steered at another
// resource server (SPEC-0007 "Access token bound to audience"). The trailing
// slash on the prefix is load-bearing: it stops a look-alike host such as
// https://cairn.stump.rocks.evil.example from matching.
func (s *Server) validResourceIndicator(res string) bool {
	if res == "" {
		return true
	}
	res = strings.TrimRight(res, "/")
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	return res == base || strings.HasPrefix(res, base+"/")
}

// handleOAuthResourceMetadata serves the protected-resource metadata (the MCP
// discovery counterpart of the AS document). Public: same bootstrap rationale.
func (s *Server) handleOAuthResourceMetadata(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, resourceMetadata{
		// The protected resource is the MCP server, whose canonical URI (per the
		// MCP authorization spec + RFC 8707) is <base>/mcp — the value clients
		// send as the `resource` indicator. Advertising the bare origin here made
		// spec-compliant clients (Crush, etc.) request <base>/mcp and get
		// invalid_target; see mcpResourceIndicator / validResourceIndicator.
		Resource:               s.mcpResourceIndicator(),
		AuthorizationServers:   []string{s.cfg.BaseURL},
		ScopesSupported:        oauth.AllScopes(),
		BearerMethodsSupported: []string{"header"},
	})
}

// --- Dynamic Client Registration (RFC 7591) ----------------------------------

type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
}

type registerResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
}

// handleOAuthRegister implements RFC 7591 dynamic client registration so an
// arbitrary MCP client (Claude Desktop, the CLI) self-registers a redirect URI
// and receives a client id. Public by the MCP auth spec's design; the OAuth
// rate limiter throttles it and every redirect URI is validated (exact-match
// registration, https or loopback http only) before anything is stored
// (SPEC-0007 REQ "Metadata Discovery & Dynamic Client Registration",
// REQ "Redirect & SSRF Validation").
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterRequestBytes)
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeOAuthError(w, r, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "registration payload too large")
			return
		}
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "registration payload is not valid JSON")
		return
	}
	// Cairn issues public clients authenticated by PKCE only ("none"); a client
	// demanding a different token-endpoint auth method is not registrable here.
	if m := req.TokenEndpointAuthMethod; m != "" && m != "none" {
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "only token_endpoint_auth_method \"none\" (public client + PKCE) is supported")
		return
	}
	for _, gt := range req.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "only authorization_code and refresh_token grant types are supported")
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "only the code response type is supported")
			return
		}
	}
	scopes, err := oauth.ParseScope(req.Scope)
	if err != nil {
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "scope must be a subset of: "+oauth.JoinScope(oauth.AllScopes()))
		return
	}
	client, err := s.oauth.RegisterClient(r.Context(), strings.TrimSpace(req.ClientName), req.RedirectURIs)
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidRedirect) {
			s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must be absolute https URIs, or http URIs on a loopback host")
			return
		}
		if errors.Is(err, oauth.ErrInvalidRequest) {
			s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "registration metadata rejected")
			return
		}
		s.log.ErrorContext(r.Context(), "oauth: register client failed", "error", err)
		s.writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	s.log.InfoContext(r.Context(), "oauth: client registered",
		"client_id", client.ID, "client_name", client.Name, "redirect_uris", len(client.RedirectURIs))
	s.writeJSON(w, http.StatusCreated, registerResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		Scope:                   oauth.JoinScope(scopes),
	})
}

// --- Authorization endpoint (login + consent) --------------------------------

// consentScopeView is one consent line: the scope value and its exact
// human-readable line (SPEC-0007 REQ "Consent Screen Content").
type consentScopeView struct {
	Value string
	Label string
}

// consentView is the consent template's view model. Client-supplied strings
// (ClientName) are rendered through html/template's contextual escaping, so a
// hostile registration cannot inject active content into Cairn's origin
// (SPEC-0007 REQ "Security Headers", scenario "Untrusted client name on the
// consent screen").
type consentView struct {
	ClientName    string
	ClientID      string
	RedirectURI   string
	Scope         string // requested scope, echoed through the form
	Scopes        []consentScopeView
	State         string
	CodeChallenge string
	CSRFToken     string
	Error         string // non-empty renders the aria-live error state instead
}

// authorizeRequest carries the validated query/form parameters of an
// authorization request.
type authorizeRequest struct {
	client      *oauth.Client
	redirectURI string
	scopes      []string
	state       string
	challenge   string
}

// validateAuthorize resolves and validates an authorization request's
// parameters from values. It distinguishes the two RFC 6749 §4.1.2.1 failure
// classes: (nil, errClient) when the client/redirect pair cannot be trusted —
// the caller MUST render an error page and redirect NOWHERE — and
// (req, errRedirect) when the pair is valid but the request is malformed, so
// the error is safely returned to the registered redirect URI.
func (s *Server) validateAuthorize(r *http.Request, values url.Values) (*authorizeRequest, string, string) {
	clientID := values.Get("client_id")
	redirectURI := values.Get("redirect_uri")
	client, err := s.oauth.GetClient(r.Context(), clientID)
	if err != nil {
		return nil, "unknown client", ""
	}
	if redirectURI == "" || !oauth.MatchRedirectURI(client.RedirectURIs, redirectURI) {
		// Never redirect to an unregistered URI (SPEC-0007 scenario "Redirect to
		// an unregistered URI").
		return nil, "redirect_uri does not match the client's registration", ""
	}
	req := &authorizeRequest{client: client, redirectURI: redirectURI, state: values.Get("state")}
	if rt := values.Get("response_type"); rt != "code" {
		return req, "", "unsupported_response_type"
	}
	req.scopes, err = oauth.ParseScope(values.Get("scope"))
	if err != nil {
		return req, "", "invalid_scope"
	}
	// PKCE is mandatory for every client, S256 only (OAuth 2.1; SPEC-0007 REQ
	// "OAuth 2.1 Authorization-Code + PKCE").
	if m := values.Get("code_challenge_method"); m != "S256" {
		return req, "", "invalid_request"
	}
	req.challenge = values.Get("code_challenge")
	if !oauth.ValidChallenge(req.challenge) {
		return req, "", "invalid_request"
	}
	// RFC 8707: an explicit resource indicator must name this server (its origin
	// or the /mcp resource URI). A foreign audience is invalid_target.
	if res := values.Get("resource"); !s.validResourceIndicator(res) {
		s.log.InfoContext(r.Context(), "oauth: authorize rejected resource indicator", "resource", res)
		return req, "", "invalid_target"
	}
	return req, "", ""
}

// redirectAuthorizeError returns an authorization error to the client's
// validated redirect URI per RFC 6749 (error + optional state).
func (s *Server) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		s.renderConsentError(w, r, "invalid redirect_uri")
		return
	}
	q := u.Query()
	q.Set("error", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// renderConsentError renders the consent screen's inline error state (an
// aria-live region announces it, SPEC-0007 Accessibility "Dynamic Content
// Regions") without redirecting anywhere.
func (s *Server) renderConsentError(w http.ResponseWriter, r *http.Request, msg string) {
	var buf strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&buf, "consent", consentView{Error: msg}); err != nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(buf.String()))
}

// sessionPrincipal resolves the request to an authenticated HUMAN web session.
// Consent is a human act: only an ambient (cookie) session qualifies — a
// bearer token, even a valid one, cannot approve a grant on the human's
// behalf (SPEC-0007 REQ "Consent Screen Content": "The human MUST be
// authenticated before the consent screen is shown").
func (s *Server) sessionPrincipal(r *http.Request) (*Principal, bool) {
	p, err := s.auth.Authenticate(r)
	if err != nil || p == nil || !p.Ambient || p.IsAgent {
		return nil, false
	}
	return p, true
}

// consentCSP returns webCSP with the client's validated redirect origin added
// to the form-action allowlist, so the browser permits the approval POST's
// redirect to the registered callback. The redirect_uri has already been
// exact-matched against the registered client (validateAuthorize), so its
// origin is trusted and adds no new attack surface; a missing/opaque origin
// falls back to the strict default.
func consentCSP(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return webCSP
	}
	return strings.Replace(webCSP, "form-action 'self'", "form-action 'self' "+u.Scheme+"://"+u.Host, 1)
}

// handleAuthorizeForm renders the consent screen for GET /oauth/authorize. An
// unauthenticated visitor is redirected to the login page with this request
// (an internal path) as the post-login destination; the consent screen itself
// rides the existing web session (issue #42: "authentication rides the
// existing web session").
func (s *Server) handleAuthorizeForm(w http.ResponseWriter, r *http.Request) {
	req, clientErr, redirErr := s.validateAuthorize(r, r.URL.Query())
	if clientErr != "" {
		s.renderConsentError(w, r, clientErr)
		return
	}
	if redirErr != "" {
		s.redirectAuthorizeError(w, r, req.redirectURI, redirErr, req.state)
		return
	}
	if _, ok := s.sessionPrincipal(r); !ok {
		http.Redirect(w, r, s.loginRedirectPath()+"?next="+url.QueryEscape(safeNext(r.URL.RequestURI())), http.StatusSeeOther)
		return
	}
	// The double-submit CSRF secret: reuse the session's readable cairn_csrf
	// cookie when present (set at login), else mint one for this form.
	csrf := ""
	if c, err := r.Cookie(csrfCookieName); err == nil {
		csrf = c.Value
	}
	if csrf == "" {
		token, err := session.NewToken()
		if err != nil {
			s.log.ErrorContext(r.Context(), "oauth: mint consent csrf failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		csrf = token
		s.setCookie(w, r, csrfCookieName, token, s.cfg.SessionTTL, false)
	}
	lines := make([]consentScopeView, 0, len(req.scopes))
	for _, sc := range req.scopes {
		lines = append(lines, consentScopeView{Value: sc, Label: oauth.ConsentLine(sc)})
	}
	// Approving the consent form redirects to the client's registered callback,
	// which is off-origin (e.g. a CLI's http://localhost:PORT/callback). WebKit
	// enforces `form-action` on the redirect target of a form submission, so the
	// default `form-action 'self'` would silently block the redirect. Widen it to
	// include the already-exact-matched redirect origin for this consent render.
	w.Header().Set("Content-Security-Policy", consentCSP(req.redirectURI))
	s.renderWeb(w, r, "consent", consentView{
		ClientName:    firstNonEmpty(req.client.Name, req.client.ID),
		ClientID:      req.client.ID,
		RedirectURI:   req.redirectURI,
		Scope:         oauth.JoinScope(req.scopes),
		Scopes:        lines,
		State:         req.state,
		CodeChallenge: req.challenge,
		CSRFToken:     csrf,
	})
}

// handleAuthorizeSubmit processes the consent decision for POST
// /oauth/authorize. It is a session-authenticated, CSRF-guarded,
// state-changing browser form (SPEC-0007 REQ "CSRF Protection", scenario
// "Cross-site consent post"): every parameter is re-validated from the posted
// fields, approval mints a single-use PKCE-bound code for exactly the approved
// scope subset, and denial returns access_denied with no grant created.
func (s *Server) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		s.renderConsentError(w, r, "malformed consent form")
		return
	}
	p, ok := s.sessionPrincipal(r)
	if !ok {
		http.Redirect(w, r, s.loginRedirectPath()+"?next="+url.QueryEscape(safeNext("/oauth/authorize")), http.StatusSeeOther)
		return
	}
	if !validFormCSRF(r) {
		s.renderWebError(w, r, errs.ErrForbidden)
		return
	}
	// Re-validate everything from the posted fields — hidden fields are as
	// untrusted as query parameters.
	req, clientErr, redirErr := s.validateAuthorize(r, r.PostForm)
	if clientErr != "" {
		s.renderConsentError(w, r, clientErr)
		return
	}
	if redirErr != "" {
		s.redirectAuthorizeError(w, r, req.redirectURI, redirErr, req.state)
		return
	}
	// Every outcome below redirects the form to the (validated) client callback;
	// widen form-action so the browser permits that off-origin navigation.
	w.Header().Set("Content-Security-Policy", consentCSP(req.redirectURI))
	if r.PostFormValue("action") != "approve" {
		// Consent denied: no grant, no token, access_denied to the client
		// (SPEC-0007 scenario "Consent denied").
		s.redirectAuthorizeError(w, r, req.redirectURI, "access_denied", req.state)
		return
	}
	// The approved subset: each checked scope must be inside the requested set;
	// approving nothing grants nothing (access_denied).
	requested := map[string]bool{}
	for _, sc := range req.scopes {
		requested[sc] = true
	}
	var approved []string
	for _, sc := range r.PostForm["approved_scope"] {
		if !requested[sc] {
			s.redirectAuthorizeError(w, r, req.redirectURI, "invalid_scope", req.state)
			return
		}
		approved = append(approved, sc)
	}
	approved, err := oauth.ParseScope(strings.Join(approved, " "))
	if err != nil || len(approved) == 0 {
		s.redirectAuthorizeError(w, r, req.redirectURI, "access_denied", req.state)
		return
	}
	code, err := s.oauth.CreateAuthCode(r.Context(), req.client.ID, p.ActorID, req.redirectURI, approved, req.challenge)
	if err != nil {
		s.log.ErrorContext(r.Context(), "oauth: create auth code failed", "client_id", req.client.ID, "error", err)
		s.redirectAuthorizeError(w, r, req.redirectURI, "server_error", req.state)
		return
	}
	s.log.InfoContext(r.Context(), "oauth: consent approved",
		"client_id", req.client.ID, "actor", p.ActorID, "scope", oauth.JoinScope(approved))
	u, _ := url.Parse(req.redirectURI)
	q := u.Query()
	q.Set("code", code)
	if req.state != "" {
		q.Set("state", req.state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// --- Token endpoint -----------------------------------------------------------

// tokenResponse is the RFC 6749 §5.1 success document.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// handleOAuthToken is POST /oauth/token: authorization-code exchange (PKCE
// verified) and refresh-token rotation. It is client-authenticated by the PKCE
// verifier / possession of the refresh token — no Cairn session — and returns
// RFC 6749-shaped errors so MCP clients interoperate (SPEC-0007 REQ "Error
// Handling Standards": distinguishable OAuth error codes).
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeOAuthError(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "request too large")
			return
		}
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	// RFC 8707: an explicit resource indicator must name this resource server
	// (its origin or the /mcp resource URI).
	if res := r.PostFormValue("resource"); !s.validResourceIndicator(res) {
		s.log.InfoContext(r.Context(), "oauth: token rejected resource indicator", "resource", res)
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	clientID := r.PostFormValue("client_id")
	var (
		set *oauth.TokenSet
		err error
	)
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		set, err = s.oauth.RedeemCode(r.Context(),
			r.PostFormValue("code"), clientID, r.PostFormValue("redirect_uri"), r.PostFormValue("code_verifier"))
	case "refresh_token":
		set, err = s.oauth.Refresh(r.Context(), r.PostFormValue("refresh_token"), clientID)
	default:
		s.writeOAuthError(w, r, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	if err != nil {
		s.writeOAuthProtocolError(w, r, err)
		return
	}
	// Token responses must never be cached (RFC 6749 §5.1).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  set.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    set.AccessExpiresIn,
		RefreshToken: set.RefreshToken,
		Scope:        set.Scope,
	})
}

// handleOAuthRevoke is POST /oauth/revoke (RFC 7009). Revoking a grant's token
// kills its whole family without touching sibling grants; an unknown or
// foreign token still yields 200 so the endpoint confirms nothing (SPEC-0007
// REQ "Token Revocation (RFC 7009)").
func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostFormValue("token")
	if token == "" {
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	if err := s.oauth.Revoke(r.Context(), token, r.PostFormValue("client_id")); err != nil {
		s.log.ErrorContext(r.Context(), "oauth: revoke failed", "error", err)
		s.writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "revocation failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- OAuth error rendering ----------------------------------------------------

// oauthErrorBody is the RFC 6749 §5.2 error document.
type oauthErrorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// writeOAuthError renders an RFC 6749-shaped error. This surface deliberately
// speaks the OAuth wire shape rather than the ADR-0012 envelope: MCP clients
// branch on the standard `error` codes. Descriptions are static strings —
// never token material or PKCE verifiers (SPEC-0007 REQ "Error Handling
// Standards": structured logging without secrets).
func (s *Server) writeOAuthError(w http.ResponseWriter, r *http.Request, status int, code, desc string) {
	s.log.InfoContext(r.Context(), "oauth: request rejected",
		"status", status, "oauth_error", code, "method", r.Method, "path", r.URL.Path)
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, status, oauthErrorBody{Error: code, ErrorDescription: desc})
}

// writeOAuthProtocolError maps a core oauth sentinel to its RFC error code.
func (s *Server) writeOAuthProtocolError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, oauth.ErrInvalidGrant):
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "the grant is invalid, expired, revoked, or failed PKCE verification")
	case errors.Is(err, oauth.ErrInvalidClient):
		s.writeOAuthError(w, r, http.StatusUnauthorized, "invalid_client", "unknown client")
	case errors.Is(err, oauth.ErrInvalidScope):
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_scope", "scope must be a subset of: "+oauth.JoinScope(oauth.AllScopes()))
	case errors.Is(err, oauth.ErrInvalidTarget):
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_target", "unknown resource")
	case errors.Is(err, oauth.ErrInvalidRequest):
		s.writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "the request is missing a required parameter")
	default:
		s.log.ErrorContext(r.Context(), "oauth: internal failure", "error", err)
		s.writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "internal error")
	}
}

// --- Bearer authentication over issued access tokens --------------------------

// OAuthAuthenticator resolves `Authorization: Bearer <access token>` against
// the authorization server's issued token families. The resulting principal is
// the on-behalf-of mapping of ADR-0004: the SUBJECT (ActorID) is the human who
// approved consent — agents inherit, never exceed, the human's reach — while
// IsAgent marks the caller as an agent so human-only capabilities (delete,
// sharing) stay refused regardless of scopes. Scopes are exactly the grant's
// approved subset. The channel is server-derived for the presenting surface.
//
// Governing: ADR-0004 (subject = human, actor = model), SPEC-0007
// REQ "Subject/Actor Identity Mapping & Least Privilege".
type OAuthAuthenticator struct {
	svc *oauth.Service
}

// Authenticate implements Authenticator. Any failure — unknown, expired,
// revoked, or audience-mismatched token — is a uniform errs.ErrUnauthorized
// (401, SPEC-0007 scenario "Revoked token used").
func (a *OAuthAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := bearerToken(r)
	if token == "" || a.svc == nil {
		return nil, errs.ErrUnauthorized
	}
	ident, err := a.svc.AuthenticateAccess(r.Context(), token)
	if err != nil {
		return nil, errs.ErrUnauthorized
	}
	scopes := make(map[string]bool, len(ident.Scopes))
	for _, sc := range ident.Scopes {
		scopes[sc] = true
	}
	return &Principal{
		ActorID: ident.ActorID,
		Channel: artifact.ChannelAPI,
		IsAgent: true,
		Scopes:  scopes,
	}, nil
}
