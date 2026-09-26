package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/session"
)

// TestTokenAuthenticatorVerifies is the core security invariant: a raw bearer
// string is NEVER trusted as an actor id. Only a registered secret authenticates,
// it resolves to the token's configured actor (not the presented text), and the
// server-derived channel is via API.
func TestTokenAuthenticatorVerifies(t *testing.T) {
	auth := NewTokenAuthenticator([]APIToken{
		{Secret: "sk_secret_alice", UserID: testTokenUserID, User: "alice"},
		{Secret: "sk_secret_bot", UserID: testTokenUserID, User: "alice", IsAgent: true},
	})

	req := func(bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}

	// A raw actor id that is NOT a registered secret is rejected — the old
	// bearer==actor impersonation is dead.
	for _, bad := range []string{"", "alice", "bob", "sk_wrong"} {
		if _, err := auth.Authenticate(req(bad)); !errors.Is(err, errs.ErrUnauthorized) {
			t.Fatalf("bearer %q must be unauthorized, got %v", bad, err)
		}
	}

	// A registered secret resolves to its configured actor, not the secret text.
	p, err := auth.Authenticate(req("sk_secret_alice"))
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if p.ActorID != "alice" || p.UserID != testTokenUserID {
		t.Fatalf("principal = %q/%q, want alice/%s (from the registry, not the token text)", p.ActorID, p.UserID, testTokenUserID)
	}
	if p.Channel != artifact.ChannelAPI {
		t.Fatalf("channel = %q, want via API (server-derived)", p.Channel)
	}
	if p.Ambient {
		t.Error("token principal must not be Ambient (CSRF-exempt)")
	}
	if !p.HasScope(scopeSharingManage) {
		t.Error("human token should carry sharing:manage")
	}

	// The agent token authenticates the same human but is capped at the ADR-0004
	// agent scopes — never sharing:manage.
	ap, err := auth.Authenticate(req("sk_secret_bot"))
	if err != nil {
		t.Fatalf("agent token: %v", err)
	}
	if !ap.IsAgent {
		t.Error("agent token must mark the principal IsAgent")
	}
	if ap.HasScope(scopeSharingManage) {
		t.Error("agent token must NOT carry sharing:manage (ADR-0004)")
	}
	if !ap.HasScope(scopeArtifactsWrite) || !ap.HasScope(scopeAnnotationsWrite) {
		t.Error("agent token should carry artifacts:write and annotations:write")
	}
}

// TestTokenAuthenticatorEmptyFailsClosed proves an unconfigured deployment (no
// tokens) rejects every bearer token, rather than defaulting open.
func TestTokenAuthenticatorEmptyFailsClosed(t *testing.T) {
	auth := NewTokenAuthenticator(nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
	r.Header.Set("Authorization", "Bearer anything")
	if _, err := auth.Authenticate(r); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("empty token registry must reject all bearers, got %v", err)
	}
}

// TestSessionAuthenticatorTokenPathVerifies proves the default web-binary auth
// seam (nil auth ⇒ SessionAuthenticator over configured tokens, dev shortcut
// OFF) verifies bearer tokens: a configured secret authenticates as its actor,
// an unregistered raw actor string does not.
func TestSessionAuthenticatorTokenPathVerifies(t *testing.T) {
	sa := &SessionAuthenticator{
		sessions: session.NewMemoryStore(),
		bearer:   NewTokenAuthenticator([]APIToken{{Secret: "sk_live", UserID: testTokenUserID, User: "sam"}}),
	}
	req := func(bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		return r
	}
	if _, err := sa.Authenticate(req("sam")); !errors.Is(err, errs.ErrUnauthorized) {
		t.Error("raw actor string must not authenticate on the token path")
	}
	p, err := sa.Authenticate(req("sk_live"))
	if err != nil {
		t.Fatalf("configured token: %v", err)
	}
	if p.ActorID != "sam" {
		t.Fatalf("actor = %q, want sam", p.ActorID)
	}
}

// TestChainAuthenticatorPrefersToken proves the verifying token authenticator
// wins over the dev shortcut when both are chained (dev mode on): a real secret
// resolves to its configured actor, while an unregistered string falls through
// to the dev actor mapping.
func TestChainAuthenticatorPrefersToken(t *testing.T) {
	chain := chainAuthenticator{
		NewTokenAuthenticator([]APIToken{{Secret: "sk_real", UserID: testTokenUserID, User: "verified"}}),
		DevActorAuthenticator{},
	}
	req := func(bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		return r
	}
	p, err := chain.Authenticate(req("sk_real"))
	if err != nil || p.ActorID != "verified" {
		t.Fatalf("token should win: %+v, %v", p, err)
	}
	// Unregistered → dev fallback maps bearer==actor.
	p, err = chain.Authenticate(req("someone"))
	if err != nil || p.ActorID != "someone" {
		t.Fatalf("dev fallback should map bearer==actor: %+v, %v", p, err)
	}
}

// TestRequireScope proves the authz middleware distinguishes 401 (no principal)
// from 403 (authenticated but under-scoped), and passes a sufficiently-scoped
// principal through.
func TestRequireScope(t *testing.T) {
	s := New(nil, nil, DevActorAuthenticator{}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var reached bool
	h := s.requireScope(scopeAnnotationsWrite)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// No principal in context → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/artifacts/x/comments", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: status = %d, want 401", rec.Code)
	}

	// Authenticated but lacks the scope → 403.
	rec = httptest.NewRecorder()
	under := &Principal{ActorID: "a", Scopes: map[string]bool{scopeArtifactsRead: true}}
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/artifacts/x/comments", nil), under))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("under-scoped: status = %d, want 403", rec.Code)
	}
	if reached {
		t.Fatal("under-scoped request must not reach the handler")
	}

	// Sufficiently scoped → passes through.
	rec = httptest.NewRecorder()
	ok := &Principal{ActorID: "a", Scopes: map[string]bool{scopeAnnotationsWrite: true}}
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/artifacts/x/comments", nil), ok))
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("scoped request should pass: status = %d, reached = %v", rec.Code, reached)
	}
}

// TestRequireHuman proves the human-only middleware refuses agent tokens (403),
// distinguishes the missing-principal 401, and passes a human principal through.
// This is the seam that keeps deletion a human-only capability (ADR-0004 /
// SPEC-0004): agents carry artifacts:write but no delete scope, so the
// write-scope gate alone would let an agent delete on the human's behalf.
func TestRequireHuman(t *testing.T) {
	s := New(nil, nil, DevActorAuthenticator{}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var reached bool
	h := s.requireHuman(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// No principal in context → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/artifacts/abc12345", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: status = %d, want 401", rec.Code)
	}

	// Agent principal → 403; the handler is never reached even though the agent
	// holds artifacts:write.
	rec = httptest.NewRecorder()
	agent := &Principal{ActorID: "alice", IsAgent: true, Scopes: agentScopes()}
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodDelete, "/v1/artifacts/abc12345", nil), agent))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent: status = %d, want 403", rec.Code)
	}
	if reached {
		t.Fatal("agent request must not reach the handler")
	}

	// Human principal (IsAgent=false) → passes through.
	rec = httptest.NewRecorder()
	human := &Principal{ActorID: "alice", Scopes: humanScopes()}
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodDelete, "/v1/artifacts/abc12345", nil), human))
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("human request should pass: status = %d, reached = %v", rec.Code, reached)
	}
}

// TestDeleteRouteRejectsAgentToken proves the wired DELETE /v1/artifacts/{id}
// route refuses an agent token with a 403 before the handler ever touches the
// store. An agent-role token (CAIRN_API_TOKENS=sk_bot:alice:agent) carries
// artifacts:write yet must never delete on the human's behalf (ADR-0004 /
// SPEC-0004 — agents receive no delete scope). The server is deliberately
// storeless: requireHuman short-circuits ahead of handleDelete, so reaching the
// nil store at all would itself be the bug this test guards against.
func TestDeleteRouteRejectsAgentToken(t *testing.T) {
	auth := NewTokenAuthenticator([]APIToken{{Secret: "sk_bot", UserID: testTokenUserID, User: "alice", IsAgent: true}})
	s := New(nil, nil, auth, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/abc12345", "sk_bot", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("agent delete: status = %d, want 403", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != errs.CodeForbidden {
		t.Fatalf("code = %q, want forbidden", env.Error.Code)
	}
}
