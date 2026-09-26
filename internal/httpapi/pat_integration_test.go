package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

// The PAT integration suite (issue #74, ADR-0004 token seam, SPEC-0007):
// create → use as bearer on /v1 → list (no secret leaked) → revoke →
// rejected; scope + owner isolation; agent-token can't delete; management
// endpoints are session-only and CSRF-guarded.
//
// Unlike sessionServer (session_integration_test.go), patServer does NOT
// enable the insecure dev bearer shortcut: this suite's whole point is that
// a *minted PAT secret* is the only thing that authenticates a bearer
// request, so leaving DevActorAuthenticator wired would let ANY string
// (including a revoked or forged secret) authenticate as itself and mask a
// real regression.
func patConfig() Config {
	return Config{
		BaseURL:          "http://cairn.test",
		MaxUploadBytes:   1 << 20,
		DefaultTTL:       time.Hour,
		DevLoginPassword: "devpass",
		SessionTTL:       time.Hour,
	}
}

func patServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, patConfig(), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// patRequestBody marshals a create-token request.
func patRequestBody(t *testing.T, name string, scopes []string, isAgent bool) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(createTokenRequest{Name: name, Scopes: scopes, IsAgent: isAgent})
	if err != nil {
		t.Fatalf("marshal create token request: %v", err)
	}
	return bytes.NewReader(b)
}

// sessionJSON issues a JSON request as the caller currently authenticated in
// client (a cookie-jarred http.Client), attaching the double-submit CSRF
// header from the session's readable cookie when present.
func sessionJSON(t *testing.T, srvURL string, client *http.Client, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srvURL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf := cookieValue(t, client, srvURL, csrfCookieName); csrf != "" {
		req.Header.Set(csrfHeaderName, csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// createPAT mints a PAT over the session-authenticated JSON API and returns
// the decoded response (which carries the one-time plaintext secret).
func createPAT(t *testing.T, srv *httptest.Server, client *http.Client, name string, scopes []string, isAgent bool) createTokenResponse {
	t.Helper()
	resp := sessionJSON(t, srv.URL, client, http.MethodPost, "/v1/tokens", patRequestBody(t, name, scopes, isAgent))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create token status = %d, body %s", resp.StatusCode, b)
	}
	var out createTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create token response: %v", err)
	}
	return out
}

// TestIntegrationPATCreateUseListRevoke is the headline round trip: mint a
// token from the web session, authenticate on /v1 with its plaintext secret,
// list it back with the secret never present, revoke it, and prove the
// revoked secret is rejected.
func TestIntegrationPATCreateUseListRevoke(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	created := createPAT(t, srv, client, "laptop agent", []string{scopeArtifactsWrite}, false)
	if created.Token == "" {
		t.Fatal("create response carried no plaintext secret")
	}
	if created.ID == "" {
		t.Fatal("create response carried no token id")
	}

	// The freshly minted secret authenticates on /v1 exactly like a
	// CAIRN_API_TOKENS entry (ADR-0004): it can create an artifact.
	createResp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=via-pat", created.Token,
		strings.NewReader("# hi from a PAT"), "text/markdown")
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create-with-PAT status = %d, body %s", createResp.StatusCode, b)
	}
	art := decodeArtifact(t, createResp)
	if art.Provenance.Actor != "sam@stump.rocks" {
		t.Fatalf("provenance actor = %q, want the PAT's owner", art.Provenance.Actor)
	}
	if art.Provenance.Channel != "via API" {
		t.Fatalf("provenance channel = %q, want via API", art.Provenance.Channel)
	}

	// List: metadata only, and the raw response body never contains the
	// plaintext secret string anywhere.
	listResp := sessionJSON(t, srv.URL, client, http.MethodGet, "/v1/tokens", nil)
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", listResp.StatusCode, listBody)
	}
	if strings.Contains(string(listBody), created.Token) {
		t.Fatal("token list response leaked the plaintext secret")
	}
	var list listTokensResponse
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the one minted token", list.Tokens)
	}

	// Revoke, then the same secret is rejected.
	delResp := sessionJSON(t, srv.URL, client, http.MethodDelete, "/v1/tokens/"+created.ID, nil)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", delResp.StatusCode)
	}
	rejected := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=after-revoke", created.Token,
		strings.NewReader("# should fail"), "text/markdown")
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-revoke request status = %d, want 401", rejected.StatusCode)
	}
}

// TestIntegrationPATScopeIsolation proves a token minted with only
// artifacts:read cannot write, and one without annotations:write cannot
// comment — the same scope enforcement any bearer principal gets.
func TestIntegrationPATScopeIsolation(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	readOnly := createPAT(t, srv, client, "read only", []string{scopeArtifactsRead}, false)

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=nope", readOnly.Token,
		strings.NewReader("# nope"), "text/markdown")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write with read-only PAT status = %d, want 403", resp.StatusCode)
	}
}

// TestIntegrationPATOwnerIsolation proves one human cannot see or revoke
// another human's tokens through the JSON API — not just at the service
// layer (already covered by internal/pat's own integration suite).
func TestIntegrationPATOwnerIsolation(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "alice@stump.rocks", "devpass").Body.Close()
	alice := createPAT(t, srv, client, "alice's token", AllScopesForTest, false)

	// A second browser session (fresh cookie jar, same server) for a
	// different human.
	bobJar, _ := cookiejar.New(nil)
	bobClient := &http.Client{
		Jar:           bobJar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	doLogin(t, srv, bobClient, "bob@stump.rocks", "devpass").Body.Close()

	listResp := sessionJSON(t, srv.URL, bobClient, http.MethodGet, "/v1/tokens", nil)
	var bobList listTokensResponse
	if err := json.NewDecoder(listResp.Body).Decode(&bobList); err != nil {
		t.Fatalf("decode bob's list: %v", err)
	}
	listResp.Body.Close()
	for _, tok := range bobList.Tokens {
		if tok.ID == alice.ID {
			t.Fatal("bob's token list contains alice's token")
		}
	}

	delResp := sessionJSON(t, srv.URL, bobClient, http.MethodDelete, "/v1/tokens/"+alice.ID, nil)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob revoking alice's token status = %d, want 404 (uniform not-found)", delResp.StatusCode)
	}

	// Alice's token is still live: bob's failed attempt changed nothing.
	stillLive := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=still-live", alice.Token,
		strings.NewReader("# still here"), "text/markdown")
	stillLive.Body.Close()
	if stillLive.StatusCode != http.StatusCreated {
		t.Fatalf("alice's token after bob's failed revoke = %d, want 201 (still live)", stillLive.StatusCode)
	}
}

// TestIntegrationPATAgentCannotDeleteOrManageSharing proves an is_agent=true
// PAT — even one holding artifacts:write — is refused delete, the one
// human-only capability the three-scope model keeps off every agent
// (ADR-0004, mirrors the equivalent OAuth-agent-token assertion).
func TestIntegrationPATAgentCannotDelete(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	agentTok := createPAT(t, srv, client, "ci bot", []string{scopeArtifactsRead, scopeArtifactsWrite, scopeAnnotationsWrite}, true)

	createResp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=agent-made", agentTok.Token,
		strings.NewReader("# made by agent"), "text/markdown")
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		createResp.Body.Close()
		t.Fatalf("agent-PAT create status = %d, body %s", createResp.StatusCode, b)
	}
	art := decodeArtifact(t, createResp)
	if art.Provenance.Actor != "sam@stump.rocks" {
		t.Fatalf("agent-created artifact actor = %q, want the owning human", art.Provenance.Actor)
	}

	delResp := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+art.ID, agentTok.Token, nil, "")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent-PAT delete status = %d, want 403", delResp.StatusCode)
	}
}

// TestIntegrationPATManagementRequiresWebSession proves a valid bearer
// token — even a valid PAT — cannot create, list, or revoke tokens: only a
// human's web session may manage credentials (issue #74:
// "session/OIDC-authenticated").
func TestIntegrationPATManagementRequiresWebSession(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()
	tok := createPAT(t, srv, client, "some token", AllScopesForTest, false)

	// A bare bearer request (no session cookie) presenting the just-minted
	// PAT secret is refused on every /v1/tokens route.
	listResp := do(t, http.MethodGet, srv.URL+"/v1/tokens", tok.Token, nil, "")
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer list tokens status = %d, want 401", listResp.StatusCode)
	}
	createResp := do(t, http.MethodPost, srv.URL+"/v1/tokens", tok.Token,
		patRequestBody(t, "should fail", AllScopesForTest, false), "application/json")
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer create token status = %d, want 401", createResp.StatusCode)
	}
	delResp := do(t, http.MethodDelete, srv.URL+"/v1/tokens/"+tok.ID, tok.Token, nil, "")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer revoke token status = %d, want 401", delResp.StatusCode)
	}
}

// TestIntegrationPATCreateRequiresCSRF proves the session-authenticated
// create endpoint is CSRF-guarded: a session-cookie request missing the
// double-submit header is refused (SPEC-0007 REQ "CSRF Protection").
func TestIntegrationPATCreateRequiresCSRF(t *testing.T) {
	srv, client := patServer(t)
	doLogin(t, srv, client, "sam@stump.rocks", "devpass").Body.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/tokens", patRequestBody(t, "no csrf", AllScopesForTest, false))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit the X-CSRF-Token header.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/tokens without csrf: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-CSRF create status = %d, want 403", resp.StatusCode)
	}
}

// AllScopesForTest is the three-scope wire form the create-token JSON API
// expects; kept local to the test package so it doesn't need to import
// internal/oauth just to name the three strings.
var AllScopesForTest = []string{scopeArtifactsRead, scopeArtifactsWrite, scopeAnnotationsWrite}
