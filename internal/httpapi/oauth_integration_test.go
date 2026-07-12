package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/store"
)

// The OAuth 2.1 authorization-server integration suite (SPEC-0007, ADR-0004,
// issue #42): full grant round trips over real HTTP against real Postgres —
// discovery, DCR, the session-authenticated consent screen, PKCE-verified code
// exchange, refresh rotation, replay/reuse revocation, and RFC 7009
// revocation. Unlike the general suite this server does NOT enable the
// insecure dev bearer shortcut: only real issued access tokens (and web
// sessions) authenticate, so 401 assertions are meaningful.

const testRedirectURI = "https://client.example/cb"

// pkceVerifier is a fixed legal code_verifier (64 unreserved chars).
const pkceVerifier = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVW-._~"

func oauthConfig() Config {
	return Config{
		BaseURL:          "http://cairn.test",
		MaxUploadBytes:   1 << 20,
		DefaultTTL:       time.Hour,
		DevLoginPassword: "devpass",
		SessionTTL:       time.Hour,
		// Generous OAuth limiter so multi-step tests never trip it; the dedicated
		// rate-limit test configures a tiny burst explicitly.
		OAuthRatePerSecond: 1000,
		OAuthRateBurst:     1000,
	}
}

// oauthServer stands up the adapter with the authorization server wired and NO
// dev bearer shortcut, plus a cookie-jarred, redirect-inspecting client for
// the browser (consent) legs.
func oauthServer(t *testing.T, cfg Config) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// registerClient performs RFC 7591 dynamic registration and returns the
// registration response.
func registerClient(t *testing.T, srv *httptest.Server, name string, redirectURIs []string) registerResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": redirectURIs})
	resp, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /oauth/register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status = %d, body %s", resp.StatusCode, b)
	}
	var reg registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if reg.ClientID == "" {
		t.Fatal("registration returned no client_id")
	}
	return reg
}

// authorizeURL builds the GET /oauth/authorize URL for a client + PKCE pair.
func authorizeURL(srv *httptest.Server, clientID, scope, state string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {oauth.S256Challenge(pkceVerifier)},
		"code_challenge_method": {"S256"},
	}
	return srv.URL + "/oauth/authorize?" + q.Encode()
}

// approveConsent drives the logged-in browser through GET consent and the
// approve POST, returning the final redirect Location.
func approveConsent(t *testing.T, srv *httptest.Server, client *http.Client, clientID, scope string, approved []string, state string) *url.URL {
	t.Helper()
	resp, err := client.Get(authorizeURL(srv, clientID, scope, state))
	if err != nil {
		t.Fatalf("GET /oauth/authorize: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent GET status = %d (Location %q), body: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	if csrf == "" {
		t.Fatal("no CSRF cookie before consent POST")
	}
	form := url.Values{
		"csrf_token":            {csrf},
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {oauth.S256Challenge(pkceVerifier)},
		"code_challenge_method": {"S256"},
		"action":                {"approve"},
	}
	for _, sc := range approved {
		form.Add("approved_scope", sc)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /oauth/authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent POST status = %d, want 303", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse consent redirect: %v", err)
	}
	return loc
}

// obtainCode runs register → login → consent-approve and returns the client id
// and the single-use authorization code.
func obtainCode(t *testing.T, srv *httptest.Server, client *http.Client, actor, scope string, approved []string) (string, string) {
	t.Helper()
	reg := registerClient(t, srv, "Test Agent", []string{testRedirectURI})
	resp := doLogin(t, srv, client, actor, "devpass")
	resp.Body.Close()
	loc := approveConsent(t, srv, client, reg.ClientID, scope, approved, "xyz-state")
	if got := loc.Query().Get("state"); got != "xyz-state" {
		t.Fatalf("state = %q, want round-tripped xyz-state", got)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in consent redirect %q", loc)
	}
	return reg.ClientID, code
}

// postToken POSTs to /oauth/token and returns the response.
func postToken(t *testing.T, srv *httptest.Server, form url.Values) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /oauth/token: %v", err)
	}
	return resp
}

// exchangeCode redeems an authorization code with the standard PKCE verifier.
func exchangeCode(t *testing.T, srv *httptest.Server, clientID, code, verifier string) *http.Response {
	t.Helper()
	return postToken(t, srv, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	})
}

func decodeToken(t *testing.T, resp *http.Response) tokenResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("token status = %d, body %s", resp.StatusCode, b)
	}
	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return tok
}

func decodeOAuthError(t *testing.T, resp *http.Response) oauthErrorBody {
	t.Helper()
	defer resp.Body.Close()
	var e oauthErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode oauth error: %v", err)
	}
	return e
}

// TestIntegrationOAuthDiscoveryMetadata proves RFC 8414 + protected-resource
// discovery resolve without auth and advertise the code+PKCE-only,
// three-scope surface (SPEC-0007 REQ "Metadata Discovery & Dynamic Client
// Registration").
func TestIntegrationOAuthDiscoveryMetadata(t *testing.T) {
	srv, _ := oauthServer(t, oauthConfig())

	resp, err := http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("GET AS metadata: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("AS metadata status = %d", resp.StatusCode)
	}
	var meta asMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatalf("decode AS metadata: %v", err)
	}
	if meta.Issuer != "http://cairn.test" ||
		meta.AuthorizationEndpoint != "http://cairn.test/oauth/authorize" ||
		meta.TokenEndpoint != "http://cairn.test/oauth/token" ||
		meta.RegistrationEndpoint != "http://cairn.test/oauth/register" ||
		meta.RevocationEndpoint != "http://cairn.test/oauth/revoke" {
		t.Fatalf("metadata endpoints wrong: %+v", meta)
	}
	if strings.Join(meta.ScopesSupported, " ") != "artifacts:read artifacts:write annotations:write" {
		t.Fatalf("scopes_supported = %v, want exactly the three", meta.ScopesSupported)
	}
	if strings.Join(meta.CodeChallengeMethodsSupported, " ") != "S256" {
		t.Fatalf("code_challenge_methods = %v, want S256 only", meta.CodeChallengeMethodsSupported)
	}
	if strings.Join(meta.ResponseTypesSupported, " ") != "code" {
		t.Fatalf("response_types = %v, want code only", meta.ResponseTypesSupported)
	}

	rres, err := http.Get(srv.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("GET resource metadata: %v", err)
	}
	defer rres.Body.Close()
	var rm resourceMetadata
	if err := json.NewDecoder(rres.Body).Decode(&rm); err != nil {
		t.Fatalf("decode resource metadata: %v", err)
	}
	if rm.Resource != "http://cairn.test" || len(rm.AuthorizationServers) != 1 {
		t.Fatalf("resource metadata wrong: %+v", rm)
	}
}

// TestIntegrationOAuthFullGrantRoundTrip is the headline acceptance (issue
// #42): a dynamically-registered client walks discovery → consent (riding the
// web session) → PKCE code exchange, then uses the issued access token as a
// real bearer on /v1 — creating an artifact owned by the human subject — while
// human-only capabilities (delete) stay refused to the agent token
// (SPEC-0007 REQ "Subject/Actor Identity Mapping & Least Privilege").
func TestIntegrationOAuthFullGrantRoundTrip(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil) // empty scope = all three, approve all

	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))
	if tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token response incomplete: %+v", tok)
	}
	if tok.Scope != "artifacts:read artifacts:write annotations:write" {
		t.Fatalf("scope = %q, want exactly the three (SPEC-0007 'Full consent grants exactly three scopes')", tok.Scope)
	}
	if tok.ExpiresIn <= 0 || tok.ExpiresIn > 3600 {
		t.Fatalf("expires_in = %d, want ~1h", tok.ExpiresIn)
	}

	// The access token is a live bearer: create an artifact as the agent.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?title=agent-made", tok.AccessToken,
		strings.NewReader("hello from the agent"), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with access token = %d, body %s", resp.StatusCode, b)
	}
	art := decodeArtifact(t, resp)
	if art.Provenance.Actor != "sam@stump.rocks" {
		t.Fatalf("provenance actor = %q, want the human subject (agents inherit the human)", art.Provenance.Actor)
	}

	// Least privilege: delete is human-only; the agent token holds
	// artifacts:write but must still be refused (403).
	del := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+art.ID, tok.AccessToken, nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusForbidden {
		t.Fatalf("agent delete = %d, want 403 (no delete capability for agents)", del.StatusCode)
	}

	// A garbage bearer stays 401 — no dev shortcut on this server.
	bad := do(t, http.MethodGet, srv.URL+"/v1/bin", "not-a-real-token", nil, "")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("garbage bearer = %d, want 401", bad.StatusCode)
	}
}

// TestIntegrationOAuthPartialConsentGrantsSubset proves approving a subset
// grants exactly that subset: a read-only grant cannot create (SPEC-0007 REQ
// "Exactly Three Consent Scopes": "exactly the approved subset and nothing
// more").
func TestIntegrationOAuthPartialConsentGrantsSubset(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", []string{"artifacts:read"})

	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))
	if tok.Scope != "artifacts:read" {
		t.Fatalf("scope = %q, want artifacts:read only", tok.Scope)
	}
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", tok.AccessToken,
		strings.NewReader("nope"), "text/plain")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create with read-only grant = %d, want 403", resp.StatusCode)
	}
	// But the granted read capability works (authenticated workspace listing).
	bin := do(t, http.MethodGet, srv.URL+"/v1/bin", tok.AccessToken, nil, "")
	bin.Body.Close()
	if bin.StatusCode != http.StatusOK {
		t.Fatalf("bin with read grant = %d, want 200", bin.StatusCode)
	}
}

// TestIntegrationOAuthBadVerifierRejected proves the token endpoint rejects an
// exchange whose PKCE verifier does not match — and that the failed attempt
// burns the single-use code (SPEC-0007 scenario "Code exchange without a
// verifier").
func TestIntegrationOAuthBadVerifierRejected(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)

	// No verifier at all.
	resp := exchangeCode(t, srv, clientID, code, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no-verifier exchange = %d, want 400", resp.StatusCode)
	}
	if e := decodeOAuthError(t, resp); e.Error != "invalid_grant" {
		t.Fatalf("no-verifier error = %q, want invalid_grant", e.Error)
	}
	// The code is burned: even the correct verifier no longer redeems it.
	resp = exchangeCode(t, srv, clientID, code, pkceVerifier)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("burned-code exchange = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// A fresh code with a WRONG verifier is also invalid_grant.
	_, code2 := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)
	resp = exchangeCode(t, srv, clientID, code2, strings.Repeat("x", 64))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-verifier exchange = %d, want 400", resp.StatusCode)
	}
	if e := decodeOAuthError(t, resp); e.Error != "invalid_grant" {
		t.Fatalf("wrong-verifier error = %q, want invalid_grant", e.Error)
	}
}

// TestIntegrationOAuthUnknownScopeRejected proves an out-of-set scope is
// refused at every door: authorization (redirect error=invalid_scope) and
// registration (SPEC-0007 scenario "Unknown or elevated scope requested").
func TestIntegrationOAuthUnknownScopeRejected(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	reg := registerClient(t, srv, "Test Agent", []string{testRedirectURI})
	resp := doLogin(t, srv, client, "sam@stump.rocks", "devpass")
	resp.Body.Close()

	// Authorization with artifacts:delete → error returned to the client.
	resp, err := client.Get(authorizeURL(srv, reg.ClientID, "artifacts:read artifacts:delete", "s1"))
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("elevated-scope authorize = %d, want 303 redirect", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), testRedirectURI) || loc.Query().Get("error") != "invalid_scope" {
		t.Fatalf("elevated-scope redirect = %q, want error=invalid_scope at the registered URI", loc)
	}
	if loc.Query().Get("code") != "" {
		t.Fatal("elevated-scope request must issue no code")
	}

	// Registration asking for an unknown scope is refused outright.
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{testRedirectURI},
		"scope":         "sharing:manage",
	})
	rres, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	if rres.StatusCode != http.StatusBadRequest {
		t.Fatalf("elevated-scope registration = %d, want 400", rres.StatusCode)
	}
	rres.Body.Close()
}

// TestIntegrationOAuthRedirectMismatchRejected proves an authorization request
// with an unregistered redirect URI renders an error page and redirects
// NOWHERE (SPEC-0007 scenario "Redirect to an unregistered URI"), and that
// registration rejects malformed/disallowed URIs (scenario "Registration with
// an invalid redirect URI").
func TestIntegrationOAuthRedirectMismatchRejected(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	reg := registerClient(t, srv, "Test Agent", []string{testRedirectURI})
	resp := doLogin(t, srv, client, "sam@stump.rocks", "devpass")
	resp.Body.Close()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {"https://evil.example/cb"},
		"code_challenge":        {oauth.S256Challenge(pkceVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched-redirect authorize = %d, want 400", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("mismatched redirect produced a Location %q — must redirect nowhere", loc)
	}

	// Registration-time rejection: http on a non-loopback host.
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://evil.example/cb"}})
	rres, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	if rres.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-redirect registration = %d, want 400", rres.StatusCode)
	}
	if e := decodeOAuthError(t, rres); e.Error != "invalid_redirect_uri" {
		t.Fatalf("bad-redirect registration error = %q, want invalid_redirect_uri", e.Error)
	}
}

// TestIntegrationOAuthReplayedCodeRevokesFamily proves a replayed code is
// rejected AND revokes the tokens the first exchange issued (SPEC-0007
// scenario "Replayed authorization code").
func TestIntegrationOAuthReplayedCodeRevokesFamily(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)

	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))

	// The token works before the replay…
	ok := do(t, http.MethodGet, srv.URL+"/v1/bin", tok.AccessToken, nil, "")
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("pre-replay bin = %d, want 200", ok.StatusCode)
	}
	// …the replay is refused…
	replay := exchangeCode(t, srv, clientID, code, pkceVerifier)
	if replay.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed code = %d, want 400", replay.StatusCode)
	}
	if e := decodeOAuthError(t, replay); e.Error != "invalid_grant" {
		t.Fatalf("replay error = %q, want invalid_grant", e.Error)
	}
	// …and the first exchange's tokens are dead (401, SPEC-0007 scenario
	// "Revoked token used").
	dead := do(t, http.MethodGet, srv.URL+"/v1/bin", tok.AccessToken, nil, "")
	dead.Body.Close()
	if dead.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-replay bin = %d, want 401", dead.StatusCode)
	}
}

// TestIntegrationOAuthRefreshRotationOverHTTP proves the refresh_token grant
// rotates the family and that reusing the rotated-out token revokes it all
// (SPEC-0007 scenario "Refresh rotation and reuse detection").
func TestIntegrationOAuthRefreshRotationOverHTTP(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)
	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))

	refreshForm := func(rt string) url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}}
	}
	rotated := decodeToken(t, postToken(t, srv, refreshForm(tok.RefreshToken)))
	if rotated.RefreshToken == tok.RefreshToken || rotated.AccessToken == tok.AccessToken {
		t.Fatal("refresh did not rotate the token family")
	}
	// The new access token works.
	ok := do(t, http.MethodGet, srv.URL+"/v1/bin", rotated.AccessToken, nil, "")
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("rotated access token bin = %d, want 200", ok.StatusCode)
	}
	// Reusing the rotated-out refresh token revokes the family…
	reuse := postToken(t, srv, refreshForm(tok.RefreshToken))
	if reuse.StatusCode != http.StatusBadRequest {
		t.Fatalf("refresh reuse = %d, want 400", reuse.StatusCode)
	}
	if e := decodeOAuthError(t, reuse); e.Error != "invalid_grant" {
		t.Fatalf("refresh reuse error = %q, want invalid_grant", e.Error)
	}
	// …killing even the freshly rotated tokens.
	dead := do(t, http.MethodGet, srv.URL+"/v1/bin", rotated.AccessToken, nil, "")
	dead.Body.Close()
	if dead.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-reuse access token = %d, want 401", dead.StatusCode)
	}
}

// TestIntegrationOAuthRevocationEndpoint proves RFC 7009: revoking one grant's
// token kills its whole family while a sibling grant keeps working, and an
// unknown token still returns 200 (SPEC-0007 REQ "Token Revocation (RFC 7009)").
func TestIntegrationOAuthRevocationEndpoint(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)
	tokA := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))

	// A sibling grant for the same human via a second consent round.
	loc := approveConsent(t, srv, client, clientID, "", oauth.AllScopes(), "state-b")
	tokB := decodeToken(t, exchangeCode(t, srv, clientID, loc.Query().Get("code"), pkceVerifier))

	revoke := func(token string) int {
		resp, err := http.Post(srv.URL+"/oauth/revoke", "application/x-www-form-urlencoded",
			strings.NewReader(url.Values{"token": {token}, "client_id": {clientID}}.Encode()))
		if err != nil {
			t.Fatalf("POST /oauth/revoke: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := revoke(tokA.RefreshToken); code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", code)
	}
	// Grant A is fully dead: access token 401, refresh invalid_grant.
	dead := do(t, http.MethodGet, srv.URL+"/v1/bin", tokA.AccessToken, nil, "")
	dead.Body.Close()
	if dead.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked grant's access token = %d, want 401", dead.StatusCode)
	}
	refresh := postToken(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokA.RefreshToken}, "client_id": {clientID}})
	refresh.Body.Close()
	if refresh.StatusCode != http.StatusBadRequest {
		t.Fatalf("revoked grant's refresh = %d, want 400", refresh.StatusCode)
	}
	// Sibling grant B keeps working (scenario "Revoke one connection").
	alive := do(t, http.MethodGet, srv.URL+"/v1/bin", tokB.AccessToken, nil, "")
	alive.Body.Close()
	if alive.StatusCode != http.StatusOK {
		t.Fatalf("sibling grant's access token = %d, want 200", alive.StatusCode)
	}
	// Unknown token: still 200, confirming nothing.
	if code := revoke("cairn_rt_completely-unknown"); code != http.StatusOK {
		t.Fatalf("revoke unknown token = %d, want 200 per RFC 7009", code)
	}
}

// TestIntegrationOAuthConsentDeniedAndCSRF proves a denial creates nothing and
// returns access_denied, and a consent POST without the CSRF token is refused
// with no grant (SPEC-0007 scenarios "Consent denied", "Cross-site consent
// post").
func TestIntegrationOAuthConsentDeniedAndCSRF(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	reg := registerClient(t, srv, "Test Agent", []string{testRedirectURI})
	resp := doLogin(t, srv, client, "sam@stump.rocks", "devpass")
	resp.Body.Close()

	baseForm := func() url.Values {
		return url.Values{
			"response_type":         {"code"},
			"client_id":             {reg.ClientID},
			"redirect_uri":          {testRedirectURI},
			"scope":                 {""},
			"state":                 {"deny-state"},
			"code_challenge":        {oauth.S256Challenge(pkceVerifier)},
			"code_challenge_method": {"S256"},
			"approved_scope":        oauth.AllScopes(),
		}
	}

	// Deny: access_denied at the registered URI, no code.
	getResp, err := client.Get(authorizeURL(srv, reg.ClientID, "", "deny-state"))
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	getResp.Body.Close()
	form := baseForm()
	form.Set("csrf_token", cookieValue(t, client, srv.URL, csrfCookieName))
	form.Set("action", "deny")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	denyResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST consent deny: %v", err)
	}
	denyResp.Body.Close()
	loc, _ := url.Parse(denyResp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny redirect = %q, want error=access_denied and no code", loc)
	}

	// Cross-site POST: no CSRF field → 403, no redirect to the client.
	form = baseForm()
	form.Set("action", "approve")
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST consent without csrf: %v", err)
	}
	csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("consent without CSRF = %d, want 403", csrfResp.StatusCode)
	}
}

// TestIntegrationOAuthConsentRequiresLoginAndEscapesClientName proves an
// unauthenticated visitor is bounced to login with the authorize request as
// ?next, and that a hostile client_name from DCR is escaped on the consent
// page (SPEC-0007 scenario "Untrusted client name on the consent screen").
func TestIntegrationOAuthConsentRequiresLoginAndEscapesClientName(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	reg := registerClient(t, srv, `<script>alert("pwn")</script>`, []string{testRedirectURI})

	// Unauthenticated: redirected to /login?next=….
	resp, err := client.Get(authorizeURL(srv, reg.ClientID, "", "s"))
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
		t.Fatalf("unauthenticated authorize = %d → %q, want 303 to /login?next=", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Log in and load consent: the client name must be escaped, its exact
	// consent lines present.
	lr := doLogin(t, srv, client, "sam@stump.rocks", "devpass")
	lr.Body.Close()
	resp, err = client.Get(authorizeURL(srv, reg.ClientID, "", "s"))
	if err != nil {
		t.Fatalf("GET authorize (authed): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if strings.Contains(page, `<script>alert("pwn")</script>`) {
		t.Fatal("client_name rendered unescaped into the consent origin")
	}
	for _, line := range []string{
		"Read artifacts you can access",
		"Create &amp; push new artifacts",
		"Comment &amp; react on your behalf",
	} {
		if !strings.Contains(page, line) {
			t.Fatalf("consent page missing line %q", line)
		}
	}
	if !strings.Contains(page, "revoke anytime in settings") {
		t.Fatal("consent page missing the revoke-in-settings notice")
	}
}

// TestIntegrationOAuthResourceMismatchAndBodyLimits proves RFC 8707 resource
// indicators are audience-checked and oversize bootstrap bodies are 413
// (SPEC-0007 REQ "Request Body Size Limits").
func TestIntegrationOAuthResourceMismatchAndBodyLimits(t *testing.T) {
	srv, client := oauthServer(t, oauthConfig())
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "", nil)

	resp := postToken(t, srv, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {pkceVerifier},
		"resource":      {"https://other.example"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign resource = %d, want 400", resp.StatusCode)
	}
	if e := decodeOAuthError(t, resp); e.Error != "invalid_target" {
		t.Fatalf("foreign resource error = %q, want invalid_target", e.Error)
	}

	// Oversize registration payload → 413 before buffering. The padding must sit
	// inside a valid JSON string so the decoder actually streams past the byte
	// cap rather than failing on invalid syntax at the first byte.
	huge := append([]byte(`{"client_name":"`), bytes.Repeat([]byte("a"), maxRegisterRequestBytes+1024)...)
	huge = append(huge, []byte(`"}`)...)
	rres, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(huge))
	if err != nil {
		t.Fatalf("POST oversize register: %v", err)
	}
	rres.Body.Close()
	if rres.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize register = %d, want 413", rres.StatusCode)
	}
}

// TestIntegrationOAuthRateLimited proves the dedicated OAuth limiter throttles
// the bootstrap endpoints with 429 + Retry-After (SPEC-0007 REQ "Rate
// Limiting").
func TestIntegrationOAuthRateLimited(t *testing.T) {
	cfg := oauthConfig()
	cfg.OAuthRatePerSecond = 0.001
	cfg.OAuthRateBurst = 2
	srv, _ := oauthServer(t, cfg)

	var last *http.Response
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]any{"redirect_uris": []string{testRedirectURI}})
		resp, err := http.Post(srv.URL+"/oauth/register", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST register #%d: %v", i, err)
		}
		if last != nil {
			last.Body.Close()
		}
		last = resp
	}
	defer last.Body.Close()
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third register = %d, want 429", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After")
	}
}
