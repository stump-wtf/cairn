package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/session"
)

// TestSafeNext proves the open-redirect defense: only single-slash-rooted
// internal paths survive; protocol-relative, absolute, and empty targets fall
// back to the Bin (SPEC-0001 REQ "Redirect & SSRF Validation").
func TestSafeNext(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "/bin"},
		{"/bin", "/bin"},
		{"/run/abc123", "/run/abc123"},
		{"/x?y=1", "/x?y=1"},
		{"//evil.example", "/bin"},
		{"https://evil.example/x", "/bin"},
		{"http://evil.example", "/bin"},
		{"javascript:alert(1)", "/bin"},
		{"/\\evil", "/bin"}, // backslash-smuggled protocol-relative, rejected
		{"ftp://x", "/bin"},
	}
	for _, tc := range tests {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDevPasswordVerifier proves login fails closed when no password is
// configured and that the shared password gates any actor id.
func TestDevPasswordVerifier(t *testing.T) {
	disabled := DevPasswordVerifier{}
	if disabled.Verify("joe", "anything") {
		t.Error("empty configured password must reject all logins")
	}
	v := DevPasswordVerifier{Password: "s3cret"}
	if !v.Verify("joe", "s3cret") {
		t.Error("correct password must authenticate")
	}
	if v.Verify("joe", "wrong") {
		t.Error("wrong password must be rejected")
	}
	if v.Verify("", "s3cret") {
		t.Error("blank actor must be rejected")
	}
	if v.Verify("joe", "") {
		t.Error("blank secret must be rejected")
	}
}

// TestSessionAuthenticatorBearerWins proves an explicit bearer token resolves to
// a non-ambient API principal even when a (mismatched) session cookie is present,
// so scripted callers keep the API's CSRF-exempt semantics.
func TestSessionAuthenticatorBearerWins(t *testing.T) {
	auth := &SessionAuthenticator{sessions: session.NewMemoryStore(), bearer: DevActorAuthenticator{}}
	r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	r.Header.Set("Authorization", "Bearer alice")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "irrelevant"})

	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.ActorID != "alice" || p.Channel != artifact.ChannelAPI {
		t.Fatalf("bearer principal = %+v, want alice/via API", p)
	}
	if p.Ambient {
		t.Error("bearer principal must not be Ambient (CSRF-exempt)")
	}
}

// TestSessionAuthenticatorCookie proves a valid session cookie resolves to an
// ambient web principal (channel via web, Ambient=true so CSRF applies), and that
// a missing/invalid cookie is unauthorized.
func TestSessionAuthenticatorCookie(t *testing.T) {
	store := session.NewMemoryStore()
	sess, err := store.Create(context.Background(), "joe", time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	auth := &SessionAuthenticator{sessions: store, bearer: DevActorAuthenticator{}}

	r := httptest.NewRequest(http.MethodGet, "/bin", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.Token})
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.ActorID != "joe" || p.Channel != artifact.ChannelWeb {
		t.Fatalf("session principal = %+v, want joe/via web", p)
	}
	if !p.Ambient {
		t.Error("session principal must be Ambient so enforceCSRF guards it")
	}
	if !p.HasScope("sharing:manage") {
		t.Error("web session must carry sharing:manage (issue #94: the Share dialog's owner controls run over the session)")
	}

	// No credentials → unauthorized.
	if _, err := auth.Authenticate(httptest.NewRequest(http.MethodGet, "/bin", nil)); err == nil {
		t.Error("no credentials must be unauthorized")
	}
	// Unknown cookie → unauthorized.
	bad := httptest.NewRequest(http.MethodGet, "/bin", nil)
	bad.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "nope"})
	if _, err := auth.Authenticate(bad); err == nil {
		t.Error("unknown session cookie must be unauthorized")
	}
}

// TestValidFormCSRF proves the no-JS double-submit check on the login/logout
// forms: the hidden field must match the readable cookie.
func TestValidFormCSRF(t *testing.T) {
	newForm := func(field, cookie string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"csrf_token": {field}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: cookie})
		}
		_ = r.ParseForm()
		return r
	}
	if validFormCSRF(newForm("", "")) {
		t.Error("empty field must fail")
	}
	if validFormCSRF(newForm("tok", "")) {
		t.Error("missing cookie must fail")
	}
	if validFormCSRF(newForm("tok", "other")) {
		t.Error("mismatched token must fail")
	}
	if !validFormCSRF(newForm("tok", "tok")) {
		t.Error("matching double-submit must pass")
	}
}

// TestRequireWebSessionRedirects proves an unauthenticated Bin request is
// redirected to login with a validated next, rather than served a JSON 401.
func TestRequireWebSessionRedirects(t *testing.T) {
	s := New(nil, nil, DevActorAuthenticator{}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := s.requireWebSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bin", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("redirect = %q, want /login?next=...", loc)
	}
}
