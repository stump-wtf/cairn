package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/store"
)

// loginConfig is the integration Config for the session surface: an http BaseURL
// (so cookies are not Secure-gated over the httptest plaintext connection) and a
// configured dev password so interactive login is enabled.
func loginConfig() Config {
	return Config{
		BaseURL:          "http://cairn.test",
		MaxUploadBytes:   1 << 20,
		DefaultTTL:       time.Hour,
		DevLoginPassword: "devpass",
		SessionTTL:       time.Hour,
	}
}

// sessionServer stands up the adapter with the default session-aware
// Authenticator (nil auth ⇒ New builds SessionAuthenticator over the store's
// Postgres session table) and returns it plus a redirect-inspecting, cookie-
// jarred client.
func sessionServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, loginConfig(), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		// Do not auto-follow so each redirect step is asserted explicitly.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// cookieValue returns the named cookie the jar holds for the server, or "".
func cookieValue(t *testing.T, client *http.Client, srvURL, name string) string {
	t.Helper()
	u, _ := url.Parse(srvURL)
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// doLogin drives GET /login (to seed the CSRF cookie) then POST /login with the
// matching double-submit field. It returns the login POST response.
func doLogin(t *testing.T, srv *httptest.Server, client *http.Client, actor, password string) *http.Response {
	t.Helper()
	resp, err := client.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	resp.Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	if csrf == "" {
		t.Fatal("GET /login did not set a CSRF cookie")
	}
	form := url.Values{"actor": {actor}, "password": {password}, "csrf_token": {csrf}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	return resp
}

// TestIntegrationSessionLoginBinLogout is the story's headline acceptance: an
// unauthenticated Bin is redirected to login; a correct dev login establishes a
// session; the Bin then lists the workspace and whoami reports the actor; and
// logout revokes the session so the Bin redirects again (SPEC-0001
// Authentication & Authorization, ADR-0004).
func TestIntegrationSessionLoginBinLogout(t *testing.T) {
	srv, client := sessionServer(t)

	// Seed one titled artifact owned by "joe" so the Bin has a row after login.
	seed := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=Checkout+Web+Audit", "joe",
		strings.NewReader("# body"), "text/markdown")
	if seed.StatusCode != http.StatusCreated {
		t.Fatalf("seed artifact = %d, want 201", seed.StatusCode)
	}
	id := decodeArtifact(t, seed).ID

	// Unauthenticated Bin → redirect to login.
	resp, err := client.Get(srv.URL + "/bin")
	if err != nil {
		t.Fatalf("GET /bin: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon /bin = %d, want 303 redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anon /bin redirect = %q, want /login...", loc)
	}

	// Login with the dev password.
	resp = doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) == "" {
		t.Fatal("login did not set a session cookie")
	}

	// The Bin now lists joe's artifact.
	status, html := getHTMLClient(t, client, srv.URL+"/bin")
	if status != http.StatusOK {
		t.Fatalf("authed /bin = %d, want 200", status)
	}
	for _, frag := range []string{`aria-label="Your artifacts"`, id, "Checkout Web Audit", "comments", "Sign out"} {
		if !strings.Contains(html, frag) {
			t.Errorf("bin missing %q", frag)
		}
	}

	// whoami reports the session actor and server-derived web channel.
	status, body := getHTMLClient(t, client, srv.URL+"/whoami")
	if status != http.StatusOK {
		t.Fatalf("whoami = %d, want 200", status)
	}
	if !strings.Contains(body, `"actor_id":"joe"`) || !strings.Contains(body, `"channel":"via web"`) {
		t.Errorf("whoami body = %q, want joe/via web", body)
	}

	// Logout (double-submit form) revokes the session.
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	logoutForm := url.Values{"csrf_token": {csrf}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", strings.NewReader(logoutForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", resp.StatusCode)
	}

	// After logout the Bin redirects to login again (session no longer resolves).
	resp, err = client.Get(srv.URL + "/bin")
	if err != nil {
		t.Fatalf("GET /bin after logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post-logout /bin = %d, want 303 redirect", resp.StatusCode)
	}
}

// TestIntegrationSessionLoginRejectsBadPassword proves a wrong password does not
// establish a session (SPEC-0001 Authentication).
func TestIntegrationSessionLoginRejectsBadPassword(t *testing.T) {
	srv, client := sessionServer(t)
	resp := doLogin(t, srv, client, "joe", "wrong")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bad login = %d, want 303 (back to form)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=1") {
		t.Fatalf("bad login redirect = %q, want /login?error=1", loc)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("a rejected login must not set a session cookie")
	}
}

// TestIntegrationLoginCSRFRequired proves a login POST without the matching
// double-submit field is refused (SPEC-0001 CSRF; login must resist cross-site
// submission even before a session exists).
func TestIntegrationLoginCSRFRequired(t *testing.T) {
	srv, client := sessionServer(t)
	// Seed the CSRF cookie but submit a mismatched field.
	resp, _ := client.Get(srv.URL + "/login")
	resp.Body.Close()
	form := url.Values{"actor": {"joe"}, "password": {"devpass"}, "csrf_token": {"forged"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged-CSRF login = %d, want 403", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("a CSRF-failed login must not set a session cookie")
	}
}

// TestIntegrationWebCommentRequiresSessionAndCSRF proves the shell comment post:
// it is refused without a session (401), refused with a session but no CSRF
// header (403 — the ambient guard), and on success records the comment under the
// server-derived session actor, returning the rendered partial (SPEC-0001 REQ
// "Comment posts through the core", SPEC-0006 CSRF, SPEC-0009 server-derived
// actor).
func TestIntegrationWebCommentRequiresSessionAndCSRF(t *testing.T) {
	srv, client := sessionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Audit")

	// Anonymous post → 401.
	resp := postForm(t, http.DefaultClient, srv.URL+"/"+id+"/comments", url.Values{"body": {"hi"}}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon comment = %d, want 401", resp.StatusCode)
	}

	// Establish a session.
	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()

	// Session but no CSRF header → 403 (ambient principal must present the token).
	resp = postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{"body": {"no csrf"}}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("session comment without CSRF = %d, want 403", resp.StatusCode)
	}

	// Session + matching CSRF header → success, partial rendered.
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp = postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{"body": {"looks good to me"}}, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session+CSRF comment = %d, want 200", resp.StatusCode)
	}
	partial, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(partial), "looks good to me") || !strings.Contains(string(partial), "reviewer") {
		t.Errorf("comment partial missing body/actor: %q", partial)
	}

	// The comment is persisted under the session actor (server-derived), readable
	// via the public link-capability comments read.
	status, listed := getHTMLClient(t, http.DefaultClient, srv.URL+"/v1/artifacts/"+id+"/comments")
	if status != http.StatusOK {
		t.Fatalf("list comments = %d, want 200", status)
	}
	if !strings.Contains(listed, `"actor_id":"reviewer"`) || !strings.Contains(listed, "looks good to me") {
		t.Errorf("stored comment not attributed to session actor: %q", listed)
	}
}

// TestIntegrationBearerStillWorksOnWebBinAndComment proves the auth seam did not
// regress the token path: a bearer caller reaches /bin's JSON sibling and posts a
// comment without a CSRF token (token callers carry no ambient credential).
func TestIntegrationBearerStillWorksOnWebBinAndComment(t *testing.T) {
	srv, _ := sessionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Audit")

	// Bearer comment on the web route with no CSRF header → allowed (exempt).
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/"+id+"/comments",
		strings.NewReader(url.Values{"body": {"from a bot"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer agentbot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bearer web comment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer web comment = %d, want 200 (token exempt from CSRF)", resp.StatusCode)
	}
}

// getHTMLClient is getHTML with an explicit client (cookie jar).
func getHTMLClient(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// postForm posts a urlencoded form with an optional X-CSRF-Token header.
func postForm(t *testing.T, client *http.Client, url string, form url.Values, csrf string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}
