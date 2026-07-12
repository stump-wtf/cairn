package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The `/` root split (#57): an authenticated caller sees their Bin, never the
// placeholder; an anonymous caller sees the on-brand landing and never touches
// Bin data. `/bin` stays live as the legacy path, and `/connect` — the "Connect
// an agent" instructions — requires the same session (SPEC-0001 REQ "Root
// Split", REQ "Connect Instructions").

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

// TestIntegrationConnectPageRequiresAuth proves the "Connect an agent" panel
// (#57) is gated exactly like the Bin: an anonymous request is redirected to
// login, and a signed-in caller sees the copy-pasteable MCP steps (server URL,
// discovery document, all three scopes, no static keys) plus the CLI
// "coming soon" note — never fabricated install steps.
func TestIntegrationConnectPageRequiresAuth(t *testing.T) {
	srv, client := sessionServer(t)
	doLogin(t, srv, client, "joe", "devpass").Body.Close()

	status, html := getHTMLClient(t, client, srv.URL+"/connect")
	if status != http.StatusOK {
		t.Fatalf("authed /connect = %d, want 200", status)
	}
	// The MCP steps are built from the server's configured public BaseURL
	// (loginConfig's "http://cairn.test"), never the httptest listener address.
	for _, frag := range []string{
		"Connect an agent",
		"http://cairn.test/.well-known/oauth-authorization-server",
		"artifacts:read",
		"artifacts:write",
		"annotations:write",
		"http://cairn.test/mcp",
		"OAuth 2.1",
		"coming soon",
		"cairn login",
		"Sign out", // shares the authenticated header chrome
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("authed /connect missing %q", frag)
		}
	}
}

// TestIntegrationConnectAnonymousRedirectsToLogin isolates the redirect
// assertion (a non-redirect-following client) so it does not depend on
// DefaultClient's redirect-following behavior.
func TestIntegrationConnectAnonymousRedirectsToLogin(t *testing.T) {
	srv, client := sessionServer(t) // client here has CheckRedirect disabled
	resp, err := client.Get(srv.URL + "/connect")
	if err != nil {
		t.Fatalf("GET /connect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon /connect = %d, want 303 redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anon /connect redirect = %q, want /login...", loc)
	}
}
