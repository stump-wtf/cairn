package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The `/` root split (#57): an authenticated caller sees their Bin, never the
// placeholder; an anonymous caller sees the on-brand landing and never touches
// Bin data. `/bin` stays live as the legacy path. `/settings` (#75) — API
// tokens, MCP connection, CLI, Account — requires the same session; `/connect`,
// the retired standalone "Connect an agent" page, is now a permanent redirect
// into it (SPEC-0001 REQ "Root Split", REQ "Settings & Connect Instructions").

// TestIntegrationRootAnonymousShowsLandingNotBin proves the logged-out root
// renders the on-brand landing — the sign-in CTA, the product pitch — and never
// leaks any workspace's Bin rows, even when artifacts exist.
func TestIntegrationRootAnonymousShowsLandingNotBin(t *testing.T) {
	srv, _ := sessionServer(t)
	createArtifact(t, srv.URL, "markdown", "joe", "# secret audit")

	status, html := getHTMLClient(t, http.DefaultClient, srv.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("anon / = %d, want 200", status)
	}
	for _, frag := range []string{
		"AI-native artifact sharing",
		`aria-label="How to push an artifact"`, // absent: proves this ISN'T the bin toolbar
	} {
		if frag == `aria-label="How to push an artifact"` {
			if strings.Contains(html, frag) {
				t.Error("anonymous root must not render the Bin toolbar")
			}
			continue
		}
		if !strings.Contains(html, frag) {
			t.Errorf("anon / missing landing fragment %q", frag)
		}
	}
	if strings.Contains(html, "secret audit") || strings.Contains(html, `aria-label="Your artifacts"`) {
		t.Error("anonymous root must not leak Bin data")
	}
	// The CTA goes to a real sign-in path (dev fallback here, since this
	// deployment has no OIDC configured).
	if !strings.Contains(html, `href="/login"`) {
		t.Error("anon landing should carry a sign-in CTA to /login (no OIDC configured in this test wiring)")
	}
}

// TestIntegrationRootAuthenticatedShowsBin proves a signed-in caller's root `/`
// renders their Bin — the same rows / chrome as the legacy `/bin` path — rather
// than the empty landing placeholder.
func TestIntegrationRootAuthenticatedShowsBin(t *testing.T) {
	srv, client := sessionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Checkout Web Audit")
	_ = id

	doLogin(t, srv, client, "joe", "devpass").Body.Close()

	status, rootHTML := getHTMLClient(t, client, srv.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("authed / = %d, want 200", status)
	}
	for _, frag := range []string{`aria-label="Your artifacts"`, "Sign out", `aria-label="How to push an artifact"`} {
		if !strings.Contains(rootHTML, frag) {
			t.Errorf("authed / missing Bin chrome fragment %q", frag)
		}
	}
	if strings.Contains(rootHTML, "instance is live") {
		t.Error("authed root must not render the old placeholder")
	}

	// `/` and `/bin` are the same projection for the same actor.
	_, binHTML := getHTMLClient(t, client, srv.URL+"/bin")
	extractRows := func(html string) string {
		i := strings.Index(html, `id="bin-rows"`)
		if i < 0 {
			return ""
		}
		return html[i:]
	}
	if extractRows(rootHTML) != extractRows(binHTML) {
		t.Error("/ and /bin should render the identical Bin rows for the same actor")
	}
}

// TestIntegrationLegacyBinPathStillResolves proves `/bin` keeps working after
// the root split, so existing bookmarks/links don't break.
func TestIntegrationLegacyBinPathStillResolves(t *testing.T) {
	srv, client := sessionServer(t)
	doLogin(t, srv, client, "joe", "devpass").Body.Close()

	status, html := getHTMLClient(t, client, srv.URL+"/bin")
	if status != http.StatusOK {
		t.Fatalf("authed /bin = %d, want 200", status)
	}
	if !strings.Contains(html, `aria-label="Your artifacts"`) {
		t.Error("/bin should still render the Bin listing")
	}
}

// TestIntegrationConnectRedirectsToSettings proves the retired standalone
// "Connect an agent" page (#57) is absorbed into Settings (#75): /connect is
// now a permanent (301) redirect to /settings, for EVERY caller — the
// redirect itself carries no auth check, matching handleConnectRedirect's
// contract that /settings (requireWebSession) is what gates the destination.
func TestIntegrationConnectRedirectsToSettings(t *testing.T) {
	srv, client := sessionServer(t) // client here has CheckRedirect disabled
	resp, err := client.Get(srv.URL + "/connect")
	if err != nil {
		t.Fatalf("GET /connect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("anon /connect = %d, want 301 redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/settings" {
		t.Fatalf("anon /connect redirect = %q, want /settings", loc)
	}

	doLogin(t, srv, client, "joe", "devpass").Body.Close()
	resp2, err := client.Get(srv.URL + "/connect")
	if err != nil {
		t.Fatalf("GET /connect (authed): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("authed /connect = %d, want 301 redirect", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/settings" {
		t.Fatalf("authed /connect redirect = %q, want /settings", loc)
	}
}

// TestIntegrationSettingsPageRequiresAuth proves the Settings page (#75) is
// gated exactly like the Bin: an anonymous request is redirected to login,
// and a signed-in caller sees the copy-pasteable MCP steps (server URL,
// discovery document, all three scopes, no static keys), the CLI install/
// login/push steps now that the CLI has shipped (#20-#22, no longer "coming
// in v0.0.3"), and the API tokens section.
func TestIntegrationSettingsPageRequiresAuth(t *testing.T) {
	srv, client := sessionServer(t)
	doLogin(t, srv, client, "joe", "devpass").Body.Close()

	status, html := getHTMLClient(t, client, srv.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("authed /settings = %d, want 200", status)
	}
	// The MCP steps are built from the server's configured public BaseURL
	// (loginConfig's "http://cairn.test"), never the httptest listener address.
	for _, frag := range []string{
		"Settings",
		"MCP connection",
		"http://cairn.test/.well-known/oauth-authorization-server",
		"artifacts:read",
		"artifacts:write",
		"annotations:write",
		"http://cairn.test/mcp",
		"OAuth 2.1",
		// The card used to advertise `go install github.com/joestump/cairn/...`,
		// a module that is not public and never resolved, alongside a link to a
		// README that 404s. It now points at the install guide, which is served
		// from this same origin (#184).
		"CLI install guide",
		"cairn login",
		"API tokens",
		"Account",
		"Sign out", // shares the authenticated header chrome
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("authed /settings missing %q", frag)
		}
	}
}

// TestIntegrationSettingsAnonymousRedirectsToLogin isolates the redirect
// assertion (a non-redirect-following client) so it does not depend on
// DefaultClient's redirect-following behavior.
func TestIntegrationSettingsAnonymousRedirectsToLogin(t *testing.T) {
	srv, client := sessionServer(t) // client here has CheckRedirect disabled
	resp, err := client.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatalf("GET /settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon /settings = %d, want 303 redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anon /settings redirect = %q, want /login...", loc)
	}
}
