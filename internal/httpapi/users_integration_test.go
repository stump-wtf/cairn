package httpapi

// Integration tests for users and identities (SPEC-0023 REQ "Users and
// Identities" and the A18 scenario of REQ "Closing the Audited Surfaces").
// Each runs against real Postgres through the real sign-in routes: a fake
// OIDC IdP (oidc_integration_test.go) and a fake GitHub below.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/stump-wtf/cairn/internal/httpapi/authprovider"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

// fakeGitHub serves the three GitHub endpoints the provider calls: the token
// exchange, GET /user and GET /user/emails.
func fakeGitHub(t *testing.T, userJSON, emailsJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": "gh-token", "token_type": "bearer"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, userJSON)
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, emailsJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// identityServer stands up the adapter with OIDC wired against idp, GitHub
// wired against gh (when non-nil), and the dev bearer shortcut on so the test
// can seed artifacts as arbitrary owners.
func identityServer(t *testing.T, idp *fakeIdP, gh *httptest.Server) *httptest.Server {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := oidcConfig()
	cfg.DevInsecureBearerAuth = true
	cfg.OIDCIssuer = idp.srv.URL
	cfg.OIDCClientID = testOIDCClientID
	cfg.OIDCClientSecret = "test-secret"
	api := New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := api.EnableOIDC(context.Background()); err != nil {
		t.Fatalf("EnableOIDC: %v", err)
	}
	if gh != nil {
		api.gh = &authprovider.GitHubProvider{
			OAuth: &oauth2.Config{
				ClientID:     "gh-client",
				ClientSecret: "gh-secret",
				RedirectURL:  cfg.BaseURL + "/auth/callback",
				Endpoint: oauth2.Endpoint{
					AuthURL:  gh.URL + "/login/oauth/authorize",
					TokenURL: gh.URL + "/login/oauth/access_token",
				},
			},
			APIBase: gh.URL,
		}
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func newJarClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// signInOIDC completes a Pocket ID sign-in on a fresh browser and returns it.
func signInOIDC(t *testing.T, srv *httptest.Server, idp *fakeIdP) *http.Client {
	t.Helper()
	client := newJarClient()
	_, loc := doOIDCLogin(t, srv, client, "")
	idp.setNonce(loc.Query().Get("nonce"))
	resp := doOIDCCallback(t, srv, client, loc.Query().Get("state"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("oidc callback = %d, want 303", resp.StatusCode)
	}
	return client
}

// signInGitHub completes a GitHub sign-in on a fresh browser and returns it.
func signInGitHub(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	client := newJarClient()
	resp, err := client.Get(srv.URL + "/auth/login?provider=github")
	if err != nil {
		t.Fatalf("GET /auth/login?provider=github: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github login start = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	q := url.Values{"state": {loc.Query().Get("state")}, "code": {"gh-code"}}
	resp, err = client.Get(srv.URL + "/auth/callback?" + q.Encode())
	if err != nil {
		t.Fatalf("github callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("github callback = %d, want 303", resp.StatusCode)
	}
	return client
}

// whoamiActor returns the actor a signed-in browser acts as.
func whoamiActor(t *testing.T, srv *httptest.Server, client *http.Client) string {
	t.Helper()
	status, body := getHTMLClient(t, client, srv.URL+"/whoami")
	if status != http.StatusOK {
		t.Fatalf("whoami = %d, want 200", status)
	}
	var who whoamiResponse
	if err := json.Unmarshal([]byte(body), &who); err != nil {
		t.Fatalf("decode whoami %q: %v", body, err)
	}
	return who.ActorID
}

// sessionBinIDs lists the artifact ids a signed-in browser's Bin returns.
func sessionBinIDs(t *testing.T, srv *httptest.Server, client *http.Client) map[string]bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/bin", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/bin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/bin = %d, want 200", resp.StatusCode)
	}
	var page binResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode bin: %v", err)
	}
	ids := map[string]bool{}
	for _, a := range page.Artifacts {
		ids[a.ID] = true
	}
	return ids
}

func seedArtifact(t *testing.T, srv *httptest.Server, owner string) string {
	t.Helper()
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=Owned", owner,
		strings.NewReader("# owned"), "text/markdown")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed as %s = %d, want 201", owner, resp.StatusCode)
	}
	return decodeArtifact(t, resp).ID
}

// SPEC-0023 "Pocket ID and GitHub, same verified email": both identities act
// as one user and see the same artifacts.
func TestIntegrationSameVerifiedEmailAcrossProviders(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setIdentity("pocket-joe", "joe@example.com", true)
	gh := fakeGitHub(t, `{"id":583231,"login":"joe-gh"}`,
		`[{"email":"Joe@Example.com","primary":true,"verified":true}]`)
	srv := identityServer(t, idp, gh)
	id := seedArtifact(t, srv, "joe@example.com")

	pocket := signInOIDC(t, srv, idp)
	github := signInGitHub(t, srv)

	pa, ga := whoamiActor(t, srv, pocket), whoamiActor(t, srv, github)
	if pa != "joe@example.com" || ga != pa {
		t.Fatalf("actors = %q (Pocket ID) and %q (GitHub), want both joe@example.com", pa, ga)
	}
	if !sessionBinIDs(t, srv, pocket)[id] || !sessionBinIDs(t, srv, github)[id] {
		t.Fatal("both identities must see the same artifacts")
	}
}

// SPEC-0023 "Unverified email cannot claim a user" (audit A5): an OIDC
// identity presenting a victim's email with email_verified false does not act
// as the victim and cannot see the victim's artifacts. Against the unfixed
// callback the actor was the raw email claim, so this fails there.
func TestIntegrationUnverifiedOIDCEmailCannotClaimUser(t *testing.T) {
	for name, verified := range map[string]any{"false": false, "string false": "false", "not a boolean": map[string]any{}} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.setIdentity("attacker-sub", "Victim@Example.com", verified)
			srv := identityServer(t, idp, nil)
			id := seedArtifact(t, srv, "victim@example.com")

			attacker := signInOIDC(t, srv, idp)
			actor := whoamiActor(t, srv, attacker)
			if strings.Contains(strings.ToLower(actor), "victim@example.com") {
				t.Fatalf("unverified email became the actor %q", actor)
			}
			if actor != "attacker-sub" {
				t.Errorf("actor = %q, want the identity's own subject", actor)
			}
			if sessionBinIDs(t, srv, attacker)[id] {
				t.Fatal("the victim's artifact is in the attacker's Bin")
			}
		})
	}
}

// A verified email sent as the string "true" (some IdPs do) still counts.
func TestIntegrationOIDCEmailVerifiedAsString(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setIdentity("pocket-sam", "Sam@Example.com", "true")
	srv := identityServer(t, idp, nil)
	if got := whoamiActor(t, srv, signInOIDC(t, srv, idp)); got != "sam@example.com" {
		t.Fatalf("actor = %q, want the lower-cased verified email", got)
	}
}

// An email-shaped subject is never trusted as an owner key without a verified
// email: it would otherwise land on that email's Bin.
func TestIntegrationEmailShapedSubjectIsNotAnOwnerKey(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setIdentity("victim@example.com", "victim@example.com", false)
	srv := identityServer(t, idp, nil)
	id := seedArtifact(t, srv, "victim@example.com")
	client := signInOIDC(t, srv, idp)
	if actor := whoamiActor(t, srv, client); !strings.HasPrefix(actor, "user:") {
		t.Fatalf("actor = %q, want a user-id key", actor)
	}
	if sessionBinIDs(t, srv, client)[id] {
		t.Fatal("an email-shaped subject reached that email's Bin")
	}
}

// SPEC-0023 "Dev login on a GitHub-only deployment" (audit A6): with GitHub
// configured, OIDC not, and a dev password set, the dev login answers 404.
func TestIntegrationDevLoginDisabledWhenGitHubConfigured(t *testing.T) {
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := loginConfig()
	cfg.GitHubClientID, cfg.GitHubClientSecret = "gh-client", "gh-secret"
	api := New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api.EnableGitHub()
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	client := newJarClient()

	resp := doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dev login with GitHub configured = %d, want 404", resp.StatusCode)
	}
	if cookieValue(t, client, srv.URL, sessionCookieName) != "" {
		t.Fatal("a disabled dev login still set a session cookie")
	}
}

// The dev login still works where no production provider exists, and its
// session is bound to a user row.
func TestIntegrationDevLoginResolvesUser(t *testing.T) {
	srv, client := sessionServer(t)
	resp := doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dev login = %d, want 303", resp.StatusCode)
	}
	if got := whoamiActor(t, srv, client); got != "joe" {
		t.Fatalf("actor = %q, want joe", got)
	}
}

// SPEC-0023 "Public read hides the creator's email" (audit A18): an anonymous
// reader of a link artifact sees a display handle, on the JSON read and on the
// web page, while the owner still sees their own email.
func TestIntegrationPublicReadHidesCreatorEmail(t *testing.T) {
	idp := newFakeIdP(t)
	gh := fakeGitHub(t, `{"id":42,"login":"samdev"}`,
		`[{"email":"sam@example.com","primary":true,"verified":true}]`)
	srv := identityServer(t, idp, gh)
	owner := signInGitHub(t, srv) // creates the user whose handle is samdev
	id := seedArtifact(t, srv, "sam@example.com")
	legacy := seedArtifact(t, srv, "pat@example.com") // no user row

	check := func(id, wantHandle, email string) {
		t.Helper()
		resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "", nil, "")
		a := decodeArtifact(t, resp)
		if a.Provenance.Actor != wantHandle {
			t.Errorf("anonymous JSON actor = %q, want %q", a.Provenance.Actor, wantHandle)
		}
		status, html := getHTML(t, srv.URL+"/"+id)
		if status != http.StatusOK {
			t.Fatalf("anonymous page = %d, want 200", status)
		}
		if strings.Contains(html, email) {
			t.Errorf("anonymous page for %s shows the creator's email %s", id, email)
		}
		if !strings.Contains(html, wantHandle) {
			t.Errorf("anonymous page for %s does not show the handle %q", id, wantHandle)
		}
	}
	check(id, "samdev", "sam@example.com")
	check(legacy, "pat", "pat@example.com")

	// Another signed-in reader is not the creator either.
	other := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "someone-else", nil, "")
	if a := decodeArtifact(t, other); a.Provenance.Actor != "samdev" {
		t.Errorf("another reader's actor = %q, want samdev", a.Provenance.Actor)
	}

	// The owner sees their own email.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/artifacts/"+id, nil)
	resp, err := owner.Do(req)
	if err != nil {
		t.Fatalf("owner GET: %v", err)
	}
	if a := decodeArtifact(t, resp); a.Provenance.Actor != "sam@example.com" {
		t.Errorf("owner's actor = %q, want sam@example.com", a.Provenance.Actor)
	}
}
