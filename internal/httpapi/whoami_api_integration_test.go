package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/store"
)

// TestIntegrationAPIWhoamiRoundTrip is the server side of cairn#21's
// "Login/whoami/logout round-trip against a test server" acceptance: a
// bearer token resolves its own actor id and server-derived channel over
// GET /v1/whoami — the round trip `cairn login` verifies a candidate token
// against and `cairn whoami` re-verifies on every invocation — while an
// absent or unknown token is uniformly unauthorized (ADR-0004).
func TestIntegrationAPIWhoamiRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := Config{
		BaseURL:        "http://cairn.test",
		MaxUploadBytes: 1 << 20,
		DefaultTTL:     time.Hour,
		SessionTTL:     time.Hour,
		APITokens:      []APIToken{{Secret: "sk_live_whoami", ActorID: "sam@stump.rocks"}},
	}
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/v1/whoami", "sk_live_whoami", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200", resp.StatusCode)
	}
	var got whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if got.ActorID != "sam@stump.rocks" {
		t.Fatalf("actor_id = %q, want sam@stump.rocks", got.ActorID)
	}
	if got.Channel != "via API" {
		t.Fatalf("channel = %q, want via API", got.Channel)
	}
	if !got.Authenticated {
		t.Fatal("authenticated = false, want true")
	}

	unauth := do(t, http.MethodGet, srv.URL+"/v1/whoami", "", nil, "")
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token whoami status = %d, want 401", unauth.StatusCode)
	}

	bad := do(t, http.MethodGet, srv.URL+"/v1/whoami", "not-a-real-token", nil, "")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-token whoami status = %d, want 401", bad.StatusCode)
	}
}
