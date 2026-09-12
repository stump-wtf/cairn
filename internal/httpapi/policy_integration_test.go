package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/store"
)

// ttlPattern loosely matches web.go's humanizeUntil output ("in 29d", "in
// 1h", "in 45m") without pinning an exact unit — humanizeDuration truncates,
// so the boundary between units shifts by the handful of milliseconds a real
// HTTP round trip takes, which an exact-string assertion would flake on.
var ttlPattern = regexp.MustCompile(`^in \d+[dhm]$`)

// policyTestConfig wires three static bearer credentials (ADR-0004 MVP token
// seam) so these tests can exercise the human-vs-agent / owner-vs-non-owner
// matrix precisely: TokenAuthenticator (not the DevActorAuthenticator dev
// shortcut, which grants every bearer only the agent scope set regardless of
// name) is what actually issues sharing:manage, and only to a human-role
// token.
func policyTestConfig() Config {
	cfg := noRateLimit()
	cfg.APITokens = []APIToken{
		{Secret: "owner-token", ActorID: "alice", IsAgent: false},      // human, owns artifacts it creates
		{Secret: "other-token", ActorID: "mallory", IsAgent: false},    // human, but never the owner below
		{Secret: "agent-token", ActorID: "alice-agent", IsAgent: true}, // agent acting for alice, no sharing:manage
	}
	return cfg
}

// createArtifactAs creates a markdown artifact as the given bearer token and
// returns its decoded envelope.
func createArtifactAs(t *testing.T, srv string, token, body string) artifactResponse {
	t.Helper()
	resp := do(t, http.MethodPost, srv+"/v1/artifacts?type=markdown&title=policy-target", token,
		strings.NewReader(body), "text/markdown")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	return decodeArtifact(t, resp)
}

func patchJSON(t *testing.T, url, token string, payload any) *http.Response {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return do(t, http.MethodPatch, url, token, strings.NewReader(string(b)), "application/json")
}

// TestIntegrationOwnerUpdatesTTL covers the issue's "owner extends/shortens
// TTL" integration scenario end-to-end over the real /v1 surface (SPEC-0009
// REQ "Default 7-Day TTL, Owner-Adjustable, Visible Countdown").
func TestIntegrationOwnerUpdatesTTL(t *testing.T) {
	srv := testServer(t, policyTestConfig(), store.Options{MaxUploadBytes: 1 << 20})
	art := createArtifactAs(t, srv.URL, "owner-token", "ttl target")
	originalExpiry := art.ExpiresAt

	// Extend to ~29 days out.
	resp := patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/ttl", "owner-token", map[string]any{"ttl_seconds": 29 * 24 * 3600})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("extend status = %d, want 200", resp.StatusCode)
	}
	extended := decodeArtifact(t, resp)
	if !extended.ExpiresAt.After(originalExpiry) {
		t.Fatalf("extended expiry %s not after original %s", extended.ExpiresAt, originalExpiry)
	}
	wantSeconds := int64(29 * 24 * 3600)
	if diff := extended.ExpiresInSeconds - wantSeconds; diff > 5 || diff < -5 {
		t.Fatalf("expires_in_seconds = %d, want ~%d", extended.ExpiresInSeconds, wantSeconds)
	}
	if !ttlPattern.MatchString(extended.ExpiresIn) {
		t.Fatalf("expires_in = %q, want a humanized countdown like \"in 29d\"", extended.ExpiresIn)
	}

	// Shorten to ~1 hour out.
	resp = patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/ttl", "owner-token", map[string]any{"ttl_seconds": 3600})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shorten status = %d, want 200", resp.StatusCode)
	}
	shortened := decodeArtifact(t, resp)
	if !shortened.ExpiresAt.Before(extended.ExpiresAt) {
		t.Fatalf("shortened expiry %s not before extended %s", shortened.ExpiresAt, extended.ExpiresAt)
	}

	// Out-of-bounds TTL is validation_failed, not silently clamped (mirrors
	// requestedTTL on create).
	resp = patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/ttl", "owner-token", map[string]any{"ttl_seconds": 999 * 24 * 3600})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-cap ttl status = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
}

// TestIntegrationNonOwnerAndAgentForbiddenFromPolicy covers the issue's "a
// non-owner or an agent/PAT without sharing:manage is 403" scenario across
// all three owner-policy endpoints (SPEC-0009 Security Requirements
// "Non-owner policy change").
func TestIntegrationNonOwnerAndAgentForbiddenFromPolicy(t *testing.T) {
	srv := testServer(t, policyTestConfig(), store.Options{MaxUploadBytes: 1 << 20})
	art := createArtifactAs(t, srv.URL, "owner-token", "forbidden target")

	cases := []struct {
		name  string
		token string
	}{
		{"non-owner human", "other-token"},
		{"agent acting for the owner", "agent-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/policy", func(t *testing.T) {
			resp := patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/policy", tc.token, map[string]any{"visibility": "private"})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if env := decodeError(t, resp); env.Error.Code != "forbidden" {
				t.Fatalf("code = %q, want forbidden", env.Error.Code)
			}
		})
		t.Run(tc.name+"/ttl", func(t *testing.T) {
			resp := patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/ttl", tc.token, map[string]any{"ttl_seconds": 3600})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
		t.Run(tc.name+"/rotate", func(t *testing.T) {
			resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+art.ID+"/rotate", tc.token, nil, "")
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}

	// The artifact must be untouched by every refused attempt.
	resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	got := decodeArtifact(t, resp)
	if got.Visibility != "link" {
		t.Fatalf("visibility = %q, want link (unchanged)", got.Visibility)
	}

	// An unauthenticated caller gets 401, not 403 — the two are distinct.
	resp = patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/policy", "", map[string]any{"visibility": "private"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
}

// TestIntegrationOwnerUpdatesVisibility covers the visibility half of "owner
// adjusts sharing" (SPEC-0009 REQ "Owner-Only Policy Changes": "Owner
// restricts to owner-only").
func TestIntegrationOwnerUpdatesVisibility(t *testing.T) {
	srv := testServer(t, policyTestConfig(), store.Options{MaxUploadBytes: 1 << 20})
	art := createArtifactAs(t, srv.URL, "owner-token", "visibility target")
	if art.Visibility != "link" {
		t.Fatalf("default visibility = %q, want link", art.Visibility)
	}

	resp := patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/policy", "owner-token", map[string]any{"visibility": "private"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	updated := decodeArtifact(t, resp)
	if updated.Visibility != "private" {
		t.Fatalf("visibility = %q, want private", updated.Visibility)
	}

	// A bare link-capability read still resolves for a private artifact —
	// visibility here governs the OWNER's sharing intent surfaced in the UI;
	// this endpoint does not itself enforce reader-side gating beyond what
	// GetByPublicID already does (link-capability read, ADR-0007) — but the
	// stored value must round-trip.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	if got := decodeArtifact(t, resp); got.Visibility != "private" {
		t.Fatalf("persisted visibility = %q, want private", got.Visibility)
	}

	// Invalid visibility value is rejected.
	resp = patchJSON(t, srv.URL+"/v1/artifacts/"+art.ID+"/policy", "owner-token", map[string]any{"visibility": "public"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus visibility status = %d, want 400", resp.StatusCode)
	}
}

// TestIntegrationRotateIDInvalidatesOldLinkPreservesAnnotations covers the
// issue's "id rotation → new id resolves, old id 404s, annotations follow"
// scenario end-to-end over the real /v1 surface (SPEC-0009 REQ "Id Rotation
// as Revoke-a-Leaked-Link").
func TestIntegrationRotateIDInvalidatesOldLinkPreservesAnnotations(t *testing.T) {
	srv := testServer(t, policyTestConfig(), store.Options{MaxUploadBytes: 1 << 20})
	art := createArtifactAs(t, srv.URL, "owner-token", "rotate target")
	oldID := art.ID

	// A comment stands in for "its annotations".
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+oldID+"/comments", "owner-token",
		strings.NewReader(`{"anchor_type":"artifact","body":"before rotation"}`), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("comment status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Owner rotates.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+oldID+"/rotate", "owner-token", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", resp.StatusCode)
	}
	rotated := decodeArtifact(t, resp)
	if rotated.ID == oldID {
		t.Fatal("rotate must mint a different public id")
	}
	if rotated.URL != "https://cairn.sh/"+rotated.ID {
		t.Fatalf("rotated url = %q, want short URL for the new id", rotated.URL)
	}

	// Old id 404s uniformly — same as a never-existed id.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+oldID, "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old id get status = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("old id code = %q, want not_found", env.Error.Code)
	}

	// New id resolves and its annotations followed it.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+rotated.ID, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new id get status = %d, want 200", resp.StatusCode)
	}
	if got := decodeArtifact(t, resp); got.CommentCount != 1 {
		t.Fatalf("comment count after rotate = %d, want 1 (annotations must follow)", got.CommentCount)
	}
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+rotated.ID+"/comments", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("comments status = %d, want 200", resp.StatusCode)
	}
	var comments commentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	resp.Body.Close()
	if len(comments.Comments) != 1 || comments.Comments[0].Body != "before rotation" {
		t.Fatalf("comments after rotate = %+v, want the pre-rotation comment", comments.Comments)
	}
}

// TestIntegrationTTLCountdownValue asserts the TTL countdown exposed on the
// artifact envelope is server-computed and correct (issue #94: "TTL
// countdown value correct").
func TestIntegrationTTLCountdownValue(t *testing.T) {
	cfg := policyTestConfig()
	cfg.DefaultTTL = 2 * time.Hour
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	art := createArtifactAs(t, srv.URL, "owner-token", "countdown target")
	want := int64((2 * time.Hour).Seconds())
	if diff := art.ExpiresInSeconds - want; diff > 5 || diff < -5 {
		t.Fatalf("expires_in_seconds = %d, want ~%d", art.ExpiresInSeconds, want)
	}
	if !ttlPattern.MatchString(art.ExpiresIn) {
		t.Fatalf("expires_in = %q, want a humanized countdown like \"in 2h\"", art.ExpiresIn)
	}

	// Re-reading the same artifact yields the same countdown (modulo elapsed
	// wall-clock time, which the ±5s tolerance above already absorbs).
	resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	got := decodeArtifact(t, resp)
	if got.ExpiresInSeconds <= 0 || got.ExpiresInSeconds > want {
		t.Fatalf("re-read expires_in_seconds = %d, want in (0, %d]", got.ExpiresInSeconds, want)
	}
}

// TestIntegrationWebSessionOwnerCanManagePolicy drives the owner policy
// surface over a REAL browser-shaped web session (cookie + double-submit
// CSRF), exactly the path the Share dialog's owner controls (share.js) use —
// not a bearer token. This is the regression test for the gap issue #94
// closed: the MVP web session previously carried no sharing:manage at all
// (session.go, pre-#94), which would have made every owner control this
// story built permanently 403 for its only real caller, a signed-in browser.
func TestIntegrationWebSessionOwnerCanManagePolicy(t *testing.T) {
	srv, client := sessionServer(t)

	// Seed an artifact owned by "joe" via the dev bearer shortcut (sessionServer
	// enables it), then log in AS joe with a real session cookie.
	seed := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=session-owned", "joe",
		strings.NewReader("session-owned body"), "text/markdown")
	if seed.StatusCode != http.StatusCreated {
		t.Fatalf("seed status = %d, want 201", seed.StatusCode)
	}
	id := decodeArtifact(t, seed).ID

	loginResp := doLogin(t, srv, client, "joe", "devpass")
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusSeeOther && loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want a redirect", loginResp.StatusCode)
	}
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	if csrf == "" {
		t.Fatal("session login did not set a CSRF cookie")
	}

	// PATCH .../policy over the session: must succeed, not 403.
	visBody, _ := json.Marshal(map[string]any{"visibility": "private"})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/artifacts/"+id+"/policy", strings.NewReader(string(visBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH policy: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session PATCH policy status = %d, want 200 (sharing:manage must be granted to the web session, issue #94)", resp.StatusCode)
	}
	if got := decodeArtifact(t, resp).Visibility; got != "private" {
		t.Fatalf("visibility = %q, want private", got)
	}

	// PATCH .../ttl over the session: must succeed too.
	ttlBody, _ := json.Marshal(map[string]any{"ttl_seconds": 3600})
	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/v1/artifacts/"+id+"/ttl", strings.NewReader(string(ttlBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("PATCH ttl: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session PATCH ttl status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// POST .../rotate over the session, without the CSRF header: refused.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/rotate", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST rotate (no csrf): %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rotate without CSRF token status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// POST .../rotate WITH the CSRF header: succeeds and mints a new id.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/rotate", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST rotate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session POST rotate status = %d, want 200", resp.StatusCode)
	}
	rotated := decodeArtifact(t, resp)
	if rotated.ID == id {
		t.Fatal("rotate must mint a different public id")
	}

	// The old id is gone, uniformly.
	old := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "", nil, "")
	if old.StatusCode != http.StatusNotFound {
		t.Fatalf("old id status = %d, want 404", old.StatusCode)
	}
	old.Body.Close()
}
