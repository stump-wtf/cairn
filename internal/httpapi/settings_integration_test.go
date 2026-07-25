package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The Settings page (issue #75): API tokens (create → shown once → listed →
// revoked), MCP connection, CLI, and Account, all behind the same session gate
// as the Bin, plus the header entry point and the accessibility/security
// posture every web shell route carries (SPEC-0001 REQ "Settings & Connect
// Instructions").

// TestIntegrationSettingsTokenLifecycleReflectsInPage is the headline
// acceptance for the API tokens section: a token minted over the JSON /v1
// surface (the same endpoints settings.js calls) appears in the
// server-rendered Settings page with its name, scopes, and an "agent" badge;
// after it is revoked over that same JSON surface, the page shows it as
// revoked with no Revoke control — proving the page and the JSON API can
// never disagree, since both read through pat.Service.List.
func TestIntegrationSettingsTokenLifecycleReflectsInPage(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	created := createPAT(t, srv, client, "laptop agent", []string{scopeArtifactsWrite, scopeAnnotationsWrite}, true)
	if created.Token == "" {
		t.Fatal("create response carried no one-time plaintext secret")
	}

	status, html := getHTMLClient(t, client, srv.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("authed /settings = %d, want 200", status)
	}
	for _, frag := range []string{
		"laptop agent",
		"artifacts:write annotations:write",
		"◆ agent",
		`data-token-revoke`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("/settings after create missing %q", frag)
		}
	}
	// The one-time plaintext itself is never server-rendered anywhere — it
	// exists only in the single POST /v1/tokens JSON response body (issue #74
	// acceptance: "list, metadata only — never the secret").
	if strings.Contains(html, created.Token) {
		t.Error("/settings must never render a token's plaintext secret")
	}

	revokeResp := sessionJSON(t, srv.URL, client, http.MethodDelete, "/v1/tokens/"+created.ID, nil)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke token status = %d, want 204", revokeResp.StatusCode)
	}

	status, html = getHTMLClient(t, client, srv.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("authed /settings after revoke = %d, want 200", status)
	}
	if !strings.Contains(html, "revoked") {
		t.Error("/settings after revoke should label the token revoked")
	}
	if strings.Contains(html, `data-token-revoke`) {
		t.Error("/settings after revoke must not still offer a Revoke control for that token")
	}
}

// TestIntegrationSettingsPageAccountSection proves the Account section shows
// the signed-in identity and how the session was established, and that the
// header's Settings entry point (the gear/avatar menu, #75) is present and
// points at /settings.
func TestIntegrationSettingsPageAccountSection(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	_, settingsHTML := getHTMLClient(t, client, srv.URL+"/settings")
	for _, frag := range []string{
		`id="account-h"`,
		"sam@stump.rocks",
		"dev password", // patConfig has no OIDC issuer wired
	} {
		if !strings.Contains(settingsHTML, frag) {
			t.Errorf("/settings Account section missing %q", frag)
		}
	}

	_, binHTML := getHTMLClient(t, client, srv.URL+"/bin")
	if !strings.Contains(binHTML, `href="/settings"`) {
		t.Error("/bin header should carry a Settings entry point (gear/avatar menu)")
	}
}

// TestIntegrationSettingsScopeOptionsMatchConsentVocabulary proves the token-
// creation form's three scope checkboxes are exactly the ADR-0004 consent
// scopes with their exact consent-screen labels — one vocabulary, reused by
// both the OAuth consent screen and this form (SPEC-0007 REQ "Exactly Three
// Consent Scopes").
func TestIntegrationSettingsScopeOptionsMatchConsentVocabulary(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	_, html := getHTMLClient(t, client, srv.URL+"/settings")
	for _, frag := range []string{
		`value="artifacts:read"`,
		`value="artifacts:write"`,
		`value="annotations:write"`,
		"Read artifacts you can access",
		"Create &amp; push new artifacts",
		"Comment &amp; react on your behalf",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("/settings token-creation scopes missing %q", frag)
		}
	}
}

// TestIntegrationSettingsPageAccessibilityAndSecurity spot-checks the WCAG
// landmark/labeling and security-header posture every web shell route carries
// (SPEC-0001 REQ "WCAG 2.1 AA & Semantics", REQ "Security Headers"), scoped to
// what is unique about Settings: the sectioned left-nav, per-field labels on
// the token-creation form, and a real table with column headers for the token
// list.
func TestIntegrationSettingsPageAccessibilityAndSecurity(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	resp, err := client.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatalf("GET /settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != webCSP {
		t.Errorf("/settings CSP = %q, want the web shell policy", csp)
	}
	_, html := getHTMLClient(t, client, srv.URL+"/settings")
	for _, frag := range []string{
		`role="banner"`,
		`role="main"`,
		`<nav class="settings-nav" aria-label="Settings sections">`,
		`for="token-name"`,
		`<th scope="col">Name</th>`,
		`aria-labelledby="tokens-h"`,
		`aria-labelledby="mcp-h"`,
		`aria-labelledby="cli-h"`,
		`aria-labelledby="account-h"`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("/settings missing accessibility fragment %q", frag)
		}
	}
}
