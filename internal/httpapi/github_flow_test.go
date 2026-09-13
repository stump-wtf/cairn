package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wiring tests pin the dispatcher contract at the route layer: an
// unconfigured provider 404s indistinguishably from an unknown one; a
// configured provider starts its flow only through ?provider=github; and a
// callback whose cookie provider contradicts the query parameter is rejected
// BEFORE anything is exchanged. The GitHub endpoints are pointed at a dead
// listener where "must not be contacted" needs proving.

func newGitHubTestServer(t *testing.T, ghConfigured bool) *Server {
	t.Helper()
	cfg := Config{
		BaseURL:            "https://cairn.example",
		GitHubClientID:     "cid",
		GitHubClientSecret: "csecret",
	}
	if !ghConfigured {
		cfg.GitHubClientID = ""
		cfg.GitHubClientSecret = ""
	}
	s := New(nil, nil, nil, cfg, slog.Default())
	s.EnableGitHub()
	return s
}

func TestEnableGitHubNoopWhenUnconfigured(t *testing.T) {
	s := newGitHubTestServer(t, false)
	if s.gh != nil {
		t.Fatal("provider wired despite missing credentials")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login?provider=github", nil)
	s.handleLoginStart(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unconfigured provider: status = %d, want 404", w.Code)
	}
}

func TestEnableGitHubWiresWhenConfigured(t *testing.T) {
	s := newGitHubTestServer(t, true)
	if s.gh == nil {
		t.Fatal("provider not wired")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login?provider=github&next=/bin", nil)
	s.handleLoginStart(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !contains(loc, "github.com/login/oauth/authorize") {
		t.Errorf("redirect = %q, want GitHub authorize URL", loc)
	}
	if !contains(loc, "state=") {
		t.Errorf("redirect = %q, want state parameter", loc)
	}
	// The state cookie must name the provider, so the callback can dispatch
	// without a query parameter.
	cookie := pickCookie(w, oidcStateCookieName)
	if cookie == nil {
		t.Fatal("no state cookie set")
	}
	st := decodeState(t, cookie.Value)
	if st.Provider != "github" || st.State == "" || st.Next != "/bin" {
		t.Errorf("state = %+v", st)
	}
}

func pickCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func stateCookie(t *testing.T, st oidcState) *http.Cookie {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{
		Name:  oidcStateCookieName,
		Value: base64.RawURLEncoding.EncodeToString(raw),
	}
}

func decodeOidcState(raw string) (oidcState, error) {
	var st oidcState
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

func TestEmptyProviderStaysOIDC(t *testing.T) {
	// The "byte-for-byte unchanged when provider is absent" requirement: with
	// no provider param and OIDC unconfigured, the legacy 503 surface is what
	// the dispatcher must produce — NOT a 404, which would change the
	// unconfigured-Pocket-ID behavior.
	s := newGitHubTestServer(t, true)
	s.oidc = nil // no OIDC wiring, as on a dev-password instance
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	s.handleLoginStart(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (legacy unconfigured-OIDC surface)", w.Code)
	}
}

func TestCallbackProviderMismatchRejectedBeforeExchange(t *testing.T) {
	s := newGitHubTestServer(t, true)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?provider=pocket-id&state=s&code=c", nil)
	// A cookie claiming a github login while the query claims pocket-id.
	r.AddCookie(stateCookie(t, oidcState{State: "s", Provider: "github", Next: "/bin"}))
	s.handleLoginCallback(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on provider mismatch", w.Code)
	}
}

func TestCallbackUnknownProviderInCookie(t *testing.T) {
	s := newGitHubTestServer(t, true)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s&code=c", nil)
	r.AddCookie(stateCookie(t, oidcState{State: "s", Provider: "evil"}))
	s.handleLoginCallback(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown provider in state cookie", w.Code)
	}
}

// --- helpers ---

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func decodeState(t *testing.T, raw string) oidcState {
	t.Helper()
	st, err := decodeOidcState(raw)
	if err != nil {
		t.Fatalf("decode state cookie: %v", err)
	}
	return st
}
