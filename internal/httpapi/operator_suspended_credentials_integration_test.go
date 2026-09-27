package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/user"
)

// TestIntegrationSuspendedUserStaticTokenAndDevLoginRefused covers the two
// authenticators TestIntegrationSuspendingUserOffboards cannot reach through
// OIDC: a CAIRN_API_TOKENS entry, and the development password login. Both
// resolve their actor string to a user, and both must refuse that user once
// users.suspended_at is set (SPEC-0023 "Suspending a user offboards them").
// Suspension revokes nothing for either (a static token is configuration and
// the dev login mints a fresh session), so the suspended flag is the only
// thing standing between the user and a working credential.
//
// The flag is set with SQL, not through the operator service, so this proves
// the authenticators alone refuse, independently of what offboarding deletes.
// The dev bearer shortcut stays off: with it on, a rejected static secret
// would still authenticate as itself.
func TestIntegrationSuspendedUserStaticTokenAndDevLoginRefused(t *testing.T) {
	const (
		suspendedActor  = "sam@stump.rocks"
		suspendedSecret = "sk_live_suspended_static"
		bystanderSecret = "sk_live_bystander_static"
	)
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := loginConfig()
	cfg.DevInsecureBearerAuth = false
	cfg.APITokens = []APIToken{
		{Secret: suspendedSecret, ActorID: suspendedActor},
		{Secret: bystanderSecret, ActorID: "bob@stump.rocks"},
	}
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	whoami := func(secret string) int {
		t.Helper()
		resp := do(t, http.MethodGet, srv.URL+"/v1/whoami", secret, nil, "")
		resp.Body.Close()
		return resp.StatusCode
	}
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	// Before suspension both credentials work, so the refusals below are the
	// flag's doing and not a misconfigured fixture.
	if got := whoami(suspendedSecret); got != http.StatusOK {
		t.Fatalf("static token before suspension: GET /v1/whoami = %d, want 200", got)
	}
	before := newClient()
	resp := doLogin(t, srv, before, suspendedActor, "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || cookieValue(t, before, srv.URL, sessionCookieName) == "" {
		t.Fatalf("dev login before suspension = %d (session cookie set: %v), want 303 and a session",
			resp.StatusCode, cookieValue(t, before, srv.URL, sessionCookieName) != "")
	}

	u, err := user.NewStore(pool).ResolveActor(context.Background(), suspendedActor)
	if err != nil {
		t.Fatalf("resolve %s: %v", suspendedActor, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE users SET suspended_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("flag suspended: %v", err)
	}

	if got := whoami(suspendedSecret); got != http.StatusUnauthorized {
		t.Errorf("suspended user's static token: GET /v1/whoami = %d, want 401", got)
	}
	if got := whoami(bystanderSecret); got != http.StatusOK {
		t.Errorf("bystander's static token: GET /v1/whoami = %d, want 200", got)
	}

	again := newClient()
	resp = doLogin(t, srv, again, suspendedActor, "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || cookieValue(t, again, srv.URL, sessionCookieName) != "" {
		t.Errorf("suspended dev login = %d (session cookie set: %v), want 403 and no session",
			resp.StatusCode, cookieValue(t, again, srv.URL, sessionCookieName) != "")
	}
}
