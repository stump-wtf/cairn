package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/trajectory"
)

func TestRateLimiterTokenBucket(t *testing.T) {
	base := time.Unix(1000, 0)
	cur := base
	rl := &rateLimiter{
		buckets: map[string]*tokenBucket{},
		rate:    1, // 1 token/sec
		burst:   2,
		now:     func() time.Time { return cur },
	}

	if ok, _ := rl.allow("k"); !ok {
		t.Fatal("1st request should be allowed (burst)")
	}
	if ok, _ := rl.allow("k"); !ok {
		t.Fatal("2nd request should be allowed (burst)")
	}
	ok, retry := rl.allow("k")
	if ok {
		t.Fatal("3rd request should be denied")
	}
	if retry <= 0 {
		t.Fatalf("denied request should report a positive Retry-After, got %v", retry)
	}
	// A different key has its own bucket.
	if ok, _ := rl.allow("other"); !ok {
		t.Fatal("independent key should be allowed")
	}
	// After 1 second, one token refills.
	cur = base.Add(time.Second)
	if ok, _ := rl.allow("k"); !ok {
		t.Fatal("after 1s a refilled token should allow the request")
	}
}

func TestRateLimiterEvictsIdleBuckets(t *testing.T) {
	base := time.Unix(1000, 0)
	cur := base
	rl := &rateLimiter{
		buckets:   map[string]*tokenBucket{},
		rate:      1,
		burst:     2,
		idleTTL:   time.Minute,
		lastSweep: base,
		now:       func() time.Time { return cur },
	}

	rl.allow("a")
	rl.allow("b")
	if len(rl.buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(rl.buckets))
	}

	// Past the idle TTL, a request for a fresh key sweeps the now-idle a and b.
	cur = base.Add(2 * time.Minute)
	rl.allow("c")
	if _, ok := rl.buckets["a"]; ok {
		t.Fatal("idle bucket a should have been evicted")
	}
	if _, ok := rl.buckets["b"]; ok {
		t.Fatal("idle bucket b should have been evicted")
	}
	if _, ok := rl.buckets["c"]; !ok {
		t.Fatal("the active bucket c must remain")
	}
}

func TestURLScheme(t *testing.T) {
	s := New(nil, nil, nil, Config{BaseURL: "https://cairn.sh/"}, slog.Default())
	tests := []struct {
		name       string
		shareType  artifact.ShareType
		id         string
		wantWebURL string
		wantMCP    string
	}{
		{"file", artifact.TypeFile, "abc123XY", "https://cairn.sh/abc123XY", "mcp://cairn/abc123XY"},
		{"markdown", sharetype.KeyMarkdown, "md345678", "https://cairn.sh/md345678", "mcp://cairn/md345678"},
		{"trajectory", sharetype.KeyTrajectory, "run45678", "https://cairn.sh/run/run45678", "mcp://cairn/run/run45678"},
		{"webhook", sharetype.KeyWebhook, "hk456789", "https://cairn.sh/hk456789", "mcp://cairn/hook/hk456789"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &artifact.Artifact{PublicID: tc.id, ShareType: tc.shareType}
			if got := s.webURL(a); got != tc.wantWebURL {
				t.Errorf("webURL = %q, want %q", got, tc.wantWebURL)
			}
			if got := s.mcpHandle(a); got != tc.wantMCP {
				t.Errorf("mcpHandle = %q, want %q", got, tc.wantMCP)
			}
		})
	}
}

func TestDevActorAuthenticator(t *testing.T) {
	a := DevActorAuthenticator{}

	// No credentials.
	if _, err := a.Authenticate(httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("missing bearer should be unauthorized, got %v", err)
	}
	// Empty token.
	r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	r.Header.Set("Authorization", "Bearer ")
	if _, err := a.Authenticate(r); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("empty bearer should be unauthorized, got %v", err)
	}
	// The dev shortcut trusts the raw token AS the actor id, server-derived
	// channel. This is the INSECURE behavior gated behind the dev flag.
	r = httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	r.Header.Set("Authorization", "Bearer alice")
	p, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("valid bearer: %v", err)
	}
	if p.ActorID != "alice" {
		t.Errorf("actor = %q, want alice", p.ActorID)
	}
	if p.Channel != artifact.ChannelAPI {
		t.Errorf("channel = %q, want %q (server-derived)", p.Channel, artifact.ChannelAPI)
	}
	if p.HasScope(scopeSharingManage) {
		t.Error("dev principal must not carry sharing:manage")
	}
}

func TestRequireAuthRejectsUnauthenticated(t *testing.T) {
	s := New(nil, nil, nil, Config{}, slog.Default())
	called := false
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil))

	if called {
		t.Fatal("wrapped handler must not run without authentication")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != errs.CodeUnauthorized {
		t.Fatalf("code = %q, want unauthorized", env.Error.Code)
	}
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		code errs.Code
		want int
	}{
		{errs.CodeNotFound, 404},
		{errs.CodeUnauthorized, 401},
		{errs.CodeForbidden, 403},
		{errs.CodeValidation, 400},
		{errs.CodeConflict, 409},
		{errs.CodePayloadTooLarge, 413},
		{errs.CodeRateLimited, 429},
		{errs.CodeInternal, 500},
		{errs.Code("weird"), 500},
	}
	for _, tc := range tests {
		if got := statusFor(tc.code); got != tc.want {
			t.Errorf("statusFor(%q) = %d, want %d", tc.code, got, tc.want)
		}
	}
}

// TestConsentCSP pins consentCSP's origin-widening behavior, including the
// #64 hardening: the origin is rebuilt from Scheme + Hostname() + a
// validated port rather than the raw (unvalidated-charset) u.Host, and any
// host that fails the hostname-charset check falls back to the strict
// webCSP instead of emitting a malformed form-action directive.
func TestConsentCSP(t *testing.T) {
	tests := []struct {
		name        string
		redirectURI string
		wantOrigin  string // "" means "falls back to plain webCSP"
	}{
		{"plain https host", "https://client.example/cb", "https://client.example"},
		{"https host with port", "https://client.example:8443/cb", "https://client.example:8443"},
		{"loopback with port", "http://127.0.0.1:53682/cb", "http://127.0.0.1:53682"},
		{"ipv6 literal with port", "http://[::1]:8080/cb", "http://[::1]:8080"},
		{"empty redirect uri", "", ""},
		{"unparsable", "http://[::1", ""},
		{"missing host", "file:///etc/passwd", ""},
		// #64: a semicolon-in-host redirect_uri (which ValidateRedirectURI
		// now rejects at registration) must never make it into a malformed
		// CSP header — consentCSP falls back to the strict default.
		{"semicolon in host", "https://a;b/c", ""},
		{"angle bracket in host", "https://a<b>.example/cb", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := consentCSP(tc.redirectURI)
			if tc.wantOrigin == "" {
				if got != webCSP {
					t.Errorf("consentCSP(%q) = %q, want plain webCSP fallback", tc.redirectURI, got)
				}
				return
			}
			want := strings.Replace(webCSP, "form-action 'self'", "form-action 'self' "+tc.wantOrigin, 1)
			if got != want {
				t.Errorf("consentCSP(%q) = %q, want %q", tc.redirectURI, got, want)
			}
			// The header must be well-formed: no bare ';' or unmatched quote
			// artifacts introduced by a bad host, and no whitespace inside
			// the injected origin token itself.
			if strings.Contains(tc.wantOrigin, " ") {
				t.Fatalf("test bug: wantOrigin %q contains a space", tc.wantOrigin)
			}
		})
	}
}

// TestOrderCategories pins the render order the legend and the time-by-category
// breakdown share: the recommended categories first in their fixed order (so a
// legend reads the same across runs), then whatever the agent invented, sorted
// so the output is deterministic rather than Go map-iteration order. Categories
// the run never used are dropped — a legend that lists unused colors is noise.
//
// Governing: ADR-0009 (open category set), SPEC-0004 REQ "Waterfall legend
// covers the categories a run used"
func TestOrderCategories(t *testing.T) {
	got := orderCategories(map[trajectory.Category]int64{
		"vibes": 5, "write": 4, "reason": 3, "deploy": 2, "search": 1, "audit": 0,
	})
	want := []trajectory.Category{
		// recommended, in RecommendedCategories order (reason … write, search …)
		"reason", "write", "search",
		// the rest, alphabetically
		"audit", "deploy", "vibes",
	}
	if len(got) != len(want) {
		t.Fatalf("orderCategories = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderCategories = %v, want %v", got, want)
		}
	}

	// A zero-duration category is still returned: the waterfall draws its spans,
	// so the legend must still explain its color. (The caller drops it from the
	// stacked bar, where a 0% segment would be invisible.)
	if got[3] != "audit" {
		t.Errorf("a 0ms category must survive ordering for the legend; got %v", got)
	}

	if len(orderCategories(map[trajectory.Category]int64{})) != 0 {
		t.Error("an empty run should produce an empty category order, not a default list")
	}
}
