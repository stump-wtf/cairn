package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
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

// --- A20: settings needs a browser session; every credential acting as the
// --- user is listed and revocable (issue #343, SPEC-0023 REQ "Closing the
// --- Audited Surfaces").

// TestIntegrationSettingsRefusesBearerTokens is the A20 scenario "Settings
// needs a browser session": a bearer token — a PAT, or an OAuth access
// token — is not a browser, and the page that lists and revokes every
// credential acting as the user must never render for one. Both are
// redirected to sign in like any other stranger.
func TestIntegrationSettingsRefusesBearerTokens(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()
	created := createPAT(t, srv, client, "laptop agent", []string{scopeArtifactsWrite}, true)

	// A PAT bearer with no browser session sees no settings page.
	patClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/settings", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	resp, err := patClient.Do(req)
	if err != nil {
		t.Fatalf("GET /settings with PAT bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /settings with a PAT bearer = %d, want 303 to sign in", resp.StatusCode)
	}

	// An OAuth access token — the other bearer shape — gets the same answer.
	browser := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	clientID, code := obtainCode(t, srv, browser, "sam@stump.rocks", "artifacts:read", []string{"artifacts:read"})
	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))
	oauthClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/settings", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = oauthClient.Do(req)
	if err != nil {
		t.Fatalf("GET /settings with OAuth bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /settings with an OAuth bearer = %d, want 303 to sign in", resp.StatusCode)
	}
}

// TestIntegrationSettingsGrantsListedAndRevoked covers the other half of
// A20: an OAuth grant used purely over REST — which never opens an MCP
// session, so the Agent sessions table cannot show it — is listed on the
// settings page and its JSON twin, and revoking it kills the whole token
// family immediately. A caller never sees another actor's grants, and an
// unknown or foreign id is a uniform 404.
func TestIntegrationSettingsGrantsListedAndRevoked(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "artifacts:read", []string{"artifacts:read"})
	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))

	// The JSON list shows the grant, with the client's registered name.
	listResp := sessionJSON(t, srv.URL, client, http.MethodGet, "/v1/oauth/grants", nil)
	body, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/oauth/grants = %d, want 200", listResp.StatusCode)
	}
	var list grantsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode grants list: %v", err)
	}
	if len(list.Grants) != 1 || list.Grants[0].Client != "Test Agent" {
		t.Fatalf("grants = %+v, want one row for Test Agent", list.Grants)
	}
	grantID := list.Grants[0].ID

	// The settings page renders the same row.
	status, html := getHTMLClient(t, client, srv.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("authed /settings = %d, want 200", status)
	}
	for _, frag := range []string{"OAuth connections", "Test Agent", `data-grant-revoke`} {
		if !strings.Contains(html, frag) {
			t.Errorf("/settings missing %q", frag)
		}
	}

	// Revoking over the JSON surface kills the grant's access token at once.
	revokeResp := sessionJSON(t, srv.URL, client, http.MethodDelete, "/v1/oauth/grants/"+grantID, nil)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke grant status = %d, want 204", revokeResp.StatusCode)
	}
	whoamiReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	whoamiReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	whoamiResp, err := http.DefaultClient.Do(whoamiReq)
	if err != nil {
		t.Fatalf("whoami with revoked token: %v", err)
	}
	whoamiResp.Body.Close()
	if whoamiResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("whoami with a revoked access token = %d, want 401", whoamiResp.StatusCode)
	}

	// The list is empty now, the page shows no live revoke button, and the
	// same revoke is a uniform 404 both again and for an unknown id.
	listResp = sessionJSON(t, srv.URL, client, http.MethodGet, "/v1/oauth/grants", nil)
	body, _ = io.ReadAll(listResp.Body)
	listResp.Body.Close()
	var empty grantsResponse
	if err := json.Unmarshal(body, &empty); err != nil {
		t.Fatalf("decode grants list after revoke: %v", err)
	}
	if len(empty.Grants) != 0 {
		t.Errorf("grants after revoke = %+v, want none", empty.Grants)
	}
	_, html = getHTMLClient(t, client, srv.URL+"/settings")
	if strings.Contains(html, `data-grant-revoke`) {
		t.Error("/settings after revoke must not still offer a Revoke control")
	}
	for _, id := range []string{grantID, "does-not-exist"} {
		resp := sessionJSON(t, srv.URL, client, http.MethodDelete, "/v1/oauth/grants/"+id, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("revoke of %q = %d, want 404", id, resp.StatusCode)
		}
	}

	// Another actor's grants are never listed to this one.
	other := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_ = other
	otherJar, _ := cookiejar.New(nil)
	otherClient := &http.Client{Jar: otherJar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, otherClient, "other@stump.rocks", "devpass").Body.Close()
	listResp = sessionJSON(t, srv.URL, otherClient, http.MethodGet, "/v1/oauth/grants", nil)
	body, _ = io.ReadAll(listResp.Body)
	listResp.Body.Close()
	var foreign grantsResponse
	if err := json.Unmarshal(body, &foreign); err != nil {
		t.Fatalf("decode other actor's grants: %v", err)
	}
	if len(foreign.Grants) != 0 {
		t.Errorf("other actor's grants = %+v, want none — grants are owner-scoped", foreign.Grants)
	}
}
