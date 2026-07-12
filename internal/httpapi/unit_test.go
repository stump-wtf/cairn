package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
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
