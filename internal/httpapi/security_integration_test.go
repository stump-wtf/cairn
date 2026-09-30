package httpapi

import (
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

// secureServer stands up the adapter in the PRODUCTION auth posture: a verifying
// static token registry and dev login, but with the insecure dev bearer shortcut
// OFF. It is the harness for asserting that no raw bearer==actor path survives.
func secureServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := Config{
		BaseURL:          "http://cairn.test",
		MaxUploadBytes:   1 << 20,
		DefaultTTL:       time.Hour,
		DevLoginPassword: "devpass",
		SessionTTL:       time.Hour,
		APITokens:        []APIToken{{Secret: "sk_live_alice", User: "alice@example.com", Position: 1}},
		// DevInsecureBearerAuth intentionally left false.
	}
	withTokenOperators(t, pool, &cfg)
	srv := httptest.NewServer(newResolvedServer(t, st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return srv, client
}

// TestIntegrationTokenAuthNoImpersonation is the end-to-end proof that the dev
// stub is gone: with the dev shortcut off, only the configured secret creates an
// artifact (as its mapped actor), while presenting the actor id itself — the old
// impersonation vector — is rejected, as is an unauthenticated create.
func TestIntegrationTokenAuthNoImpersonation(t *testing.T) {
	srv, _ := secureServer(t)

	// The configured secret authenticates AS its mapped actor.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=t", "sk_live_alice",
		strings.NewReader("# body"), "text/markdown")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("configured token create = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.Provenance.Actor != "alice@example.com" {
		t.Fatalf("actor = %q, want alice@example.com (from the token registry)", art.Provenance.Actor)
	}
	if art.Provenance.Channel != "via API" {
		t.Fatalf("channel = %q, want via API", art.Provenance.Channel)
	}

	// Presenting the user reference as a bearer (the old bearer==actor
	// trust) → 401.
	for _, bearer := range []string{"alice", "alice@example.com"} {
		resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown", bearer,
			strings.NewReader("# body"), "text/markdown")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("impersonation via Bearer %s = %d, want 401", bearer, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// An unknown secret → 401.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown", "sk_not_real",
		strings.NewReader("# body"), "text/markdown")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown secret = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// No credentials → 401.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown", "",
		strings.NewReader("# body"), "text/markdown")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationCreateCSRFAlignment proves the create endpoint now closes the
// CSRF gap for ambient (cookie-session) callers, while token callers stay
// exempt: a browser session cannot be cross-site-tricked into creating an
// artifact without the double-submit token, but a scripted bearer caller needs
// none (SPEC-0006 REQ "CSRF Protection"; ADR-0004).
func TestIntegrationCreateCSRFAlignment(t *testing.T) {
	srv, client := secureServer(t)

	// Establish a browser session for alice.
	resp := doLogin(t, srv, client, "alice", "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}

	post := func(withCSRF bool) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=t", strings.NewReader("# body"))
		req.Header.Set("Content-Type", "text/markdown")
		if withCSRF {
			req.Header.Set(csrfHeaderName, cookieValue(t, client, srv.URL, csrfCookieName))
		}
		r, err := client.Do(req) // cookie jar attaches the session + csrf cookies
		if err != nil {
			t.Fatalf("POST /v1/artifacts: %v", err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	// Ambient caller without the CSRF header → 403 (the gap this story closes).
	if got := post(false); got != http.StatusForbidden {
		t.Fatalf("session create without CSRF = %d, want 403", got)
	}
	// Ambient caller with the matching double-submit token → 201.
	if got := post(true); got != http.StatusCreated {
		t.Fatalf("session create with CSRF = %d, want 201", got)
	}

	// A token (non-ambient) caller is exempt — no CSRF header, still 201.
	tokResp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=t", "sk_live_alice",
		strings.NewReader("# body"), "text/markdown")
	if tokResp.StatusCode != http.StatusCreated {
		t.Fatalf("token create (CSRF-exempt) = %d, want 201", tokResp.StatusCode)
	}
	tokResp.Body.Close()
}
