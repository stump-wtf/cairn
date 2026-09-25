// The OIDC relying-party integration suite (ADR-0013, issue #55): full login
// round trips over real HTTP against real Postgres, driven against a hermetic
// in-test OIDC issuer (discovery + JWKS + token endpoint, RS256-signed ID
// tokens) so no live Pocket ID is needed. Mirrors ~/src/switchboard's fake-IdP
// test harness (internal/auth/oidc_test.go), adapted to Cairn's session store
// and its OAuth 2.1 consent screen / MCP transport, which this suite also
// proves are unaffected by the new login mechanism.
package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

const (
	testOIDCClientID = "cairn"
	testOIDCSubject  = "pocket|test-sub"
	testOIDCEmail    = "human@example.com"
)

// --- fake IdP ------------------------------------------------------------

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that mints RS256-signed ID tokens. Tests steer it via nonce (echoed into the
// minted token), expiresIn (defaults to +1h; set negative for an expired
// token), and badSignature (sign with a key absent from the JWKS).
type fakeIdP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	wrongKey *rsa.PrivateKey

	mu           sync.Mutex
	nonce        string
	expiresIn    time.Duration
	badSignature bool
	noEmail      bool
	lastVerifier string
	// subject, email and emailVerified override the minted identity; zero
	// values mean testOIDCSubject, testOIDCEmail and email_verified: true.
	subject       string
	email         string
	emailVerified any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate wrong RSA key: %v", err)
	}
	f := &fakeIdP{key: key, wrongKey: wrongKey, expiresIn: time.Hour}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
				"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.lastVerifier = r.PostFormValue("code_verifier")
		nonce := f.nonce
		expiresIn := f.expiresIn
		noEmail := f.noEmail
		subject := firstNonEmpty(f.subject, testOIDCSubject)
		email := firstNonEmpty(f.email, testOIDCEmail)
		var emailVerified any = true
		if f.emailVerified != nil {
			emailVerified = f.emailVerified
		}
		signKey := f.key
		if f.badSignature {
			signKey = f.wrongKey
		}
		f.mu.Unlock()
		now := time.Now()
		claims := map[string]any{
			"iss":   f.srv.URL,
			"sub":   subject,
			"aud":   testOIDCClientID,
			"exp":   now.Add(expiresIn).Unix(),
			"iat":   now.Unix(),
			"nonce": nonce,
		}
		if !noEmail {
			claims["email"] = email
			claims["email_verified"] = emailVerified
		}
		idToken := signJWT(t, signKey, claims)
		writeJSON(w, map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) setNonce(n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce = n
}

func (f *fakeIdP) setExpiresIn(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiresIn = d
}

func (f *fakeIdP) setBadSignature(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.badSignature = v
}

func (f *fakeIdP) setNoEmail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noEmail = v
}

// setIdentity steers the next minted ID token's sub, email and
// email_verified claim (a bool, or a string for IdPs that send "true").
func (f *fakeIdP) setIdentity(subject, email string, emailVerified any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subject, f.email, f.emailVerified = subject, email, emailVerified
}

func (f *fakeIdP) verifierSeen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastVerifier
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// signJWT mints a compact RS256 JWT (header.payload.signature) for the fake IdP.
func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// --- server + client helpers ----------------------------------------------

// oidcConfig is the integration Config for the OIDC surface: an http BaseURL
// (cookies stay non-Secure over the plaintext httptest connection) and a
// DevLoginPassword deliberately set too, so tests can prove it is refused
// once OIDC is wired (ADR-0013's demotion), not merely absent.
func oidcConfig() Config {
	return Config{
		BaseURL:            "http://cairn.test",
		MaxUploadBytes:     1 << 20,
		DefaultTTL:         time.Hour,
		DevLoginPassword:   "devpass",
		SessionTTL:         time.Hour,
		OAuthRatePerSecond: 1000,
		OAuthRateBurst:     1000,
	}
}

// oidcServer stands up the adapter over real Postgres with OIDC wired against
// idp (EnableOIDC runs real discovery against the fake IdP's httptest server),
// and returns it plus a redirect-inspecting, cookie-jarred client.
func oidcServer(t *testing.T, idp *fakeIdP, cfg Config) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg.OIDCIssuer = idp.srv.URL
	cfg.OIDCClientID = testOIDCClientID
	cfg.OIDCClientSecret = "test-secret"
	api := New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := api.EnableOIDC(context.Background()); err != nil {
		t.Fatalf("EnableOIDC: %v", err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// doOIDCLogin drives GET /auth/login and returns the response (a 302 to the
// fake IdP) plus the parsed redirect target.
func doOIDCLogin(t *testing.T, srv *httptest.Server, client *http.Client, next string) (*http.Response, *url.URL) {
	t.Helper()
	u := srv.URL + "/auth/login"
	if next != "" {
		u += "?next=" + url.QueryEscape(next)
	}
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/login = %d, want 302", resp.StatusCode)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	return resp, loc
}

// doOIDCCallback replays the IdP redirect: GET /auth/callback?state=...&code=...
// (the state cookie rides along in the client's jar).
func doOIDCCallback(t *testing.T, srv *httptest.Server, client *http.Client, state string) *http.Response {
	t.Helper()
	q := url.Values{"state": {state}, "code": {"test-code"}}
	resp, err := client.Get(srv.URL + "/auth/callback?" + q.Encode())
	if err != nil {
		t.Fatalf("GET /auth/callback: %v", err)
	}
	return resp
}

// --- happy path -------------------------------------------------------------

// TestIntegrationOIDCLoginRoundTripEstablishesSession proves the full flow:
// /auth/login redirects to the IdP with state+nonce+PKCE, the callback
// exchanges the code (presenting the stashed PKCE verifier), verifies the ID
// token, and establishes an ordinary Cairn session — the same session cookie
// and CSRF cookie a dev-password login mints — under the ID token's email
// claim. The post-login redirect honors the validated ?next=.
func TestIntegrationOIDCLoginRoundTripEstablishesSession(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "/bin")
	if got, want := loc.Scheme+"://"+loc.Host+loc.Path, idp.srv.URL+"/authorize"; got != want {
		t.Fatalf("redirect target = %s, want %s", got, want)
	}
	q := loc.Query()
	if q.Get("client_id") != testOIDCClientID {
		t.Fatalf("client_id = %q, want %q", q.Get("client_id"), testOIDCClientID)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("PKCE challenge missing: %v", q)
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("state/nonce missing: %v", q)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope must include openid: %q", q.Get("scope"))
	}

	idp.setNonce(q.Get("nonce"))
	resp := doOIDCCallback(t, srv, client, q.Get("state"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/bin" {
		t.Fatalf("post-login redirect = %q, want /bin (the validated ?next)", loc)
	}

	// The PKCE verifier presented at the token endpoint must be the one stashed
	// at /auth/login, not a fresh/empty one.
	if idp.verifierSeen() == "" {
		t.Fatal("token exchange must present the stashed PKCE verifier")
	}

	// A real session + CSRF cookie now exist, resolving to the ID token's email.
	if cookieValue(t, client, srv.URL, sessionCookieName) == "" {
		t.Fatal("callback did not set a session cookie")
	}
	status, body := getHTMLClient(t, client, srv.URL+"/whoami")
	if status != http.StatusOK {
		t.Fatalf("whoami = %d, want 200", status)
	}
	if !strings.Contains(body, `"actor_id":"`+testOIDCEmail+`"`) {
		t.Fatalf("whoami body = %q, want actor_id %s", body, testOIDCEmail)
	}

	// The state cookie is single-use: it must be cleared after the callback.
	if cookieValue(t, client, srv.URL, oidcStateCookieName) != "" {
		t.Fatal("oidc state cookie must be cleared after a successful callback")
	}
}

// TestIntegrationOIDCFallsBackToSubjectWithNoEmailClaim proves the actor id
// falls back to the OIDC subject when the ID token asserts no email — the
// documented fallback (ADR-0013, issue #55 "prefer email; fall back to sub").
func TestIntegrationOIDCFallsBackToSubjectWithNoEmailClaim(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setNoEmail(true)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	resp := doOIDCCallback(t, srv, client, loc.Query().Get("state"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", resp.StatusCode)
	}

	status, body := getHTMLClient(t, client, srv.URL+"/whoami")
	if status != http.StatusOK {
		t.Fatalf("whoami = %d, want 200", status)
	}
	if !strings.Contains(body, `"actor_id":"`+testOIDCSubject+`"`) {
		t.Fatalf("whoami body = %q, want actor_id %s (subject fallback)", body, testOIDCSubject)
	}
}

// --- state / nonce / token validation ---------------------------------------

// TestIntegrationOIDCStateMismatchRejected proves a tampered state parameter is
// rejected before any code exchange, and no session is minted (issue #55
// checklist "state mismatch rejected").
func TestIntegrationOIDCStateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	doOIDCLogin(t, srv, client, "")
	resp := doOIDCCallback(t, srv, client, "tampered-state")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if idp.verifierSeen() != "" {
		t.Fatal("state mismatch must abort before the code exchange")
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("state mismatch must not establish a session")
	}
}

// TestIntegrationOIDCCallbackWithoutLoginRejected proves a bare callback (no
// prior /auth/login, so no state cookie) is rejected.
func TestIntegrationOIDCCallbackWithoutLoginRejected(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	resp := doOIDCCallback(t, srv, client, "whatever")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestIntegrationOIDCNonceMismatchRejected proves the ID token verifies but its
// nonce does not match the stashed nonce is rejected with no session minted.
func TestIntegrationOIDCNonceMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce("attacker-supplied-nonce") // IdP echoes a different nonce than stashed

	resp := doOIDCCallback(t, srv, client, loc.Query().Get("state"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("nonce mismatch must not establish a session")
	}
}

// TestIntegrationOIDCExpiredIDTokenRejected proves an expired ID token fails
// verification and mints no session (issue #55 checklist "expired ... ID token
// rejected").
func TestIntegrationOIDCExpiredIDTokenRejected(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	idp.setExpiresIn(-time.Hour) // already expired when minted

	resp := doOIDCCallback(t, srv, client, loc.Query().Get("state"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("an expired id_token must not establish a session")
	}
}

// TestIntegrationOIDCInvalidSignatureRejected proves an ID token signed by a
// key outside the issuer's published JWKS fails verification and mints no
// session (issue #55 checklist "invalid ID token rejected").
func TestIntegrationOIDCInvalidSignatureRejected(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	idp.setBadSignature(true)

	resp := doOIDCCallback(t, srv, client, loc.Query().Get("state"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("a bad id_token signature must not establish a session")
	}
}

// --- web-route + login-page behavior -----------------------------------------

// TestIntegrationOIDCUnauthenticatedWebRouteRedirectsToAuthLogin proves an
// unauthenticated session-gated web route redirects straight to /auth/login —
// no intermediate form — whenever OIDC is configured (issue #55 checklist
// "unauthenticated web route redirects to /auth/login"; ADR-0013
// loginRedirectPath).
func TestIntegrationOIDCUnauthenticatedWebRouteRedirectsToAuthLogin(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	resp, err := client.Get(srv.URL + "/bin")
	if err != nil {
		t.Fatalf("GET /bin: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon /bin = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
		t.Fatalf("anon /bin redirect = %q, want /auth/login...", loc)
	}
}

// TestIntegrationOIDCLoginPageShowsPocketIDAction proves GET /login itself
// (reachable directly, e.g. a stale bookmark) renders the "Sign in with Pocket
// ID" action and NOT the dev-password form once OIDC is configured.
func TestIntegrationOIDCLoginPageShowsPocketIDAction(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	status, body := getHTMLClient(t, client, srv.URL+"/login")
	if status != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", status)
	}
	if !strings.Contains(body, "Sign in with Pocket ID") {
		t.Errorf("login page missing the OIDC action: %q", body)
	}
	if !strings.Contains(body, `href="/auth/login`) {
		t.Errorf("login page's OIDC action must link to /auth/login: %q", body)
	}
	if strings.Contains(body, `name="password"`) {
		t.Errorf("login page must not render the dev-password form once OIDC is configured: %q", body)
	}
}

// TestIntegrationOIDCConfiguredRejectsDevPassword proves dev_login_password is
// refused once OIDC is configured — the ADR-0013 demotion, not merely an
// absent credential (issue #55 checklist "dev_login_password still works when
// OIDC unset" — this is its converse: it must NOT work when OIDC IS set).
func TestIntegrationOIDCConfiguredRejectsDevPassword(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	resp := doLogin(t, srv, client, "joe", "devpass")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dev-password login with OIDC configured = %d, want 404", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("a refused dev-password login must not establish a session")
	}
}

// --- interaction with the OAuth 2.1 consent screen + MCP --------------------

// TestIntegrationOIDCSessionSatisfiesOAuthConsent proves an OIDC-established
// session is a fully ordinary web session from the /oauth/authorize consent
// screen's point of view (ADR-0004's sessionPrincipal needed zero changes):
// login via OIDC, then register an MCP client and approve consent exactly as a
// dev-password session would (issue #55 checklist "/oauth/authorize consent
// subject is the OIDC-authenticated human").
func TestIntegrationOIDCSessionSatisfiesOAuthConsent(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := oidcConfig()
	cfg.OAuthRatePerSecond = 1000
	cfg.OAuthRateBurst = 1000
	srv, client := oidcServer(t, idp, cfg)

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	doOIDCCallback(t, srv, client, loc.Query().Get("state")).Body.Close()
	if cookieValue(t, client, srv.URL, sessionCookieName) == "" {
		t.Fatal("OIDC login did not establish a session")
	}

	reg := registerClient(t, srv, "OIDC Test Agent", []string{testRedirectURI})
	loc2 := approveConsent(t, srv, client, reg.ClientID,
		"artifacts:read artifacts:write annotations:write",
		[]string{"artifacts:read", "artifacts:write", "annotations:write"}, "oidc-consent-state")
	if loc2.Query().Get("code") == "" {
		t.Fatalf("consent did not yield an authorization code: %v", loc2)
	}
}

// TestIntegrationOIDCUnauthenticatedConsentRedirectsToAuthLogin proves the
// consent screen's own unauthenticated redirect also goes straight to
// /auth/login when OIDC is configured — the same loginRedirectPath seam
// requireWebSession uses.
func TestIntegrationOIDCUnauthenticatedConsentRedirectsToAuthLogin(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	reg := registerClient(t, srv, "Anon Test Agent", []string{testRedirectURI})
	resp, err := client.Get(authorizeURL(srv, reg.ClientID, "artifacts:read", "s"))
	if err != nil {
		t.Fatalf("GET /oauth/authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon consent = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
		t.Fatalf("anon consent redirect = %q, want /auth/login...", loc)
	}
}

// TestIntegrationOIDCSessionDoesNotGrantMCPAccess proves an OIDC-established
// session cookie is not itself sufficient to reach /mcp — the transport
// authenticates OAuth access tokens only (mcpTokenVerifier), completely
// independent of the web session cookie (issue #55 checklist "/mcp remain[s]
// ... OAuth/bearer only").
func TestIntegrationOIDCSessionDoesNotGrantMCPAccess(t *testing.T) {
	idp := newFakeIdP(t)
	srv, client := oidcServer(t, idp, oidcConfig())

	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	doOIDCCallback(t, srv, client, loc.Query().Get("state")).Body.Close()
	if cookieValue(t, client, srv.URL, sessionCookieName) == "" {
		t.Fatal("OIDC login did not establish a session")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) // the cookie jar attaches the OIDC session cookie automatically
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/mcp with only a web session cookie = %d, want 401 (bearer/OAuth only)", resp.StatusCode)
	}
}
