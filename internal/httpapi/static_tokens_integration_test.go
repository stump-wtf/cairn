package httpapi

// Integration tests for SPEC-0023 REQ "Static API Tokens Act as an Operator's
// User": CAIRN_API_TOKENS entries name an operator's existing user, resolved
// at boot; anything else fails boot by position and never by secret; a token
// acts as that user and nobody else; and its user's Settings page lists it
// as operator-provisioned.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/operator"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/user"
)

// testTokenIssuer is the issuer withTokenOperators gives the identities it
// creates for email-named tokens.
const testTokenIssuer = "https://tokens.cairn.test"

// withTokenOperators makes every CAIRN_API_TOKENS entry in cfg name an
// operator who has signed in: it creates each named user with a sign-in
// identity (an "<issuer>|<subject>" as itself; an email as testTokenIssuer
// with that email verified) and lists those identities in cfg.Operators. It
// is for tests of what a token may DO; who may hold one is tested below. A
// cfg that already names its operators is left alone.
func withTokenOperators(t *testing.T, pool *pgxpool.Pool, cfg *Config) {
	t.Helper()
	if len(cfg.APITokens) == 0 || cfg.Operators != nil {
		return
	}
	users := user.NewStore(pool)
	var ids []string
	for _, tok := range cfg.APITokens {
		id := user.Identity{Issuer: testTokenIssuer, Subject: tok.User, Email: tok.User, EmailVerified: true}
		if issuer, subject, ok := strings.Cut(tok.User, "|"); ok {
			id = user.Identity{Issuer: issuer, Subject: subject}
		}
		if _, err := users.Resolve(context.Background(), id); err != nil {
			t.Fatalf("create token user: %v", err)
		}
		ids = append(ids, id.Issuer+"|"+id.Subject)
	}
	set, err := operator.Parse(strings.Join(ids, ","), "")
	if err != nil {
		t.Fatalf("operator.Parse: %v", err)
	}
	cfg.Operators = set
}

// newResolvedServer builds the adapter over st exactly as cairnd does:
// construct, then resolve CAIRN_API_TOKENS, failing the test on a boot error.
func newResolvedServer(t *testing.T, st *store.Store, cfg Config, logger *slog.Logger) *Server {
	t.Helper()
	api := New(st, nil, nil, cfg, logger)
	if err := api.ResolveAPITokens(context.Background()); err != nil {
		t.Fatalf("ResolveAPITokens: %v", err)
	}
	return api
}

// syncBuffer is a goroutine-safe log sink: the server logs from its request
// goroutines while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const (
	opTokenSecret      = "sk_operator_human_7f3a9c1e5b2d4f6a8c0e"
	opAgentTokenSecret = "sk_operator_agent_2b4d6f8a0c1e3a5c7e9b"
)

// tokenOperatorServer stands up the adapter with OIDC against idp, the
// operator named by (idp, opSubject), and two tokens naming that operator:
// entry 1 by identity with the human role, entry 2 by verified email with
// the default (agent) role. The dev bearer is off: it would accept any
// rejected secret as an actor of its own. Nothing is resolved yet: the
// operator has not signed in.
func tokenOperatorServer(t *testing.T, idp *fakeIdP, logs io.Writer) (*httptest.Server, *Server, *pgxpool.Pool) {
	t.Helper()
	opIdentity := idp.srv.URL + "|" + opSubject
	set, err := operator.Parse(opIdentity, "")
	if err != nil {
		t.Fatalf("operator.Parse: %v", err)
	}
	tokens, err := ParseAPITokens(opTokenSecret + ":" + opIdentity + ":human, " + opAgentTokenSecret + ":" + opEmail)
	if err != nil {
		t.Fatalf("ParseAPITokens: %v", err)
	}
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := oidcConfig()
	cfg.OIDCIssuer = idp.srv.URL
	cfg.OIDCClientID = testOIDCClientID
	cfg.OIDCClientSecret = "test-secret"
	cfg.Operators = set
	cfg.APITokens = tokens
	api := New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := api.EnableOIDC(context.Background()); err != nil {
		t.Fatalf("EnableOIDC: %v", err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, api, pool
}

// whoamiAs returns the actor and status GET /v1/whoami reports for a bearer.
func whoamiAs(t *testing.T, srv *httptest.Server, bearer string) (string, int) {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+"/v1/whoami", bearer, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var who whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	return who.ActorID, resp.StatusCode
}

// SPEC-0023 "Token cannot impersonate", end to end: a token resolves only
// once its operator has signed in, then acts as that operator's user, and
// an artifact it creates is owned by that user whatever the request claims.
// The operator's Settings page lists both tokens as operator-provisioned, by
// position; no one else's does, and no page or log line carries a secret.
func TestIntegrationStaticTokenActsOnlyAsItsOperator(t *testing.T) {
	idp := newFakeIdP(t)
	logs := &syncBuffer{}
	srv, api, pool := tokenOperatorServer(t, idp, logs)
	ctx := context.Background()

	// Before the operator has ever signed in there is no user to act as:
	// boot fails on entry 1, and no token authenticates.
	err := api.ResolveAPITokens(ctx)
	if err == nil || err.Error() != "CAIRN_API_TOKENS entry 1: "+errTokenNoUser.Error() {
		t.Fatalf("resolve before sign-in = %v, want entry 1: %v", err, errTokenNoUser)
	}
	if _, status := whoamiAs(t, srv, opTokenSecret); status == http.StatusOK {
		t.Fatal("a token authenticated before it was resolved")
	}

	op := signInAs(t, srv, idp, opSubject, opEmail, nil)
	victim := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	if err := api.ResolveAPITokens(ctx); err != nil {
		t.Fatalf("resolve after sign-in: %v", err)
	}
	for _, secret := range []string{opTokenSecret, opAgentTokenSecret} {
		if actor, status := whoamiAs(t, srv, secret); status != http.StatusOK || actor != opEmail {
			t.Fatalf("token whoami = %q (%d), want %s", actor, status, opEmail)
		}
	}

	// Create with every way a request might try to name someone else.
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/artifacts?type=markdown&title=by-token", strings.NewReader("# token"))
	req.Header.Set("Authorization", "Bearer "+opTokenSecret)
	req.Header.Set("Content-Type", "text/markdown")
	for _, h := range []string{"X-Cairn-Actor", "X-Actor-Id", "X-On-Behalf-Of", "X-Forwarded-User"} {
		req.Header.Set(h, victimEmail)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("token create = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.Provenance.Actor != opEmail {
		t.Errorf("provenance actor = %q, want %s", art.Provenance.Actor, opEmail)
	}
	var ownerEmail, creatorEmail string
	if err := pool.QueryRow(ctx, `
		SELECT o.primary_email, c.primary_email
		  FROM artifacts a
		  JOIN users o ON o.id = a.owner_user_id
		  JOIN users c ON c.id = a.created_by_user_id
		 WHERE a.public_id = $1`, art.ID).Scan(&ownerEmail, &creatorEmail); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if ownerEmail != opEmail || creatorEmail != opEmail {
		t.Fatalf("owner/creator = %s/%s, want the operator %s", ownerEmail, creatorEmail, opEmail)
	}
	if !sessionBinIDs(t, srv, op)[art.ID] || sessionBinIDs(t, srv, victim)[art.ID] {
		t.Fatal("the token's artifact is not in exactly the operator's Bin")
	}

	// The default role is agent: no sharing:manage, no delete.
	vis := do(t, http.MethodPatch, srv.URL+"/v1/artifacts/"+art.ID+"/policy", opAgentTokenSecret,
		strings.NewReader(`{"visibility":"private"}`), "application/json")
	vis.Body.Close()
	if vis.StatusCode != http.StatusForbidden {
		t.Errorf("agent-role token policy change = %d, want 403", vis.StatusCode)
	}
	del := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+art.ID, opAgentTokenSecret, nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusForbidden {
		t.Errorf("agent-role token delete = %d, want 403", del.StatusCode)
	}

	// A token is never an operator session.
	assertOperatorRoutes404(t, srv, nil, opTokenSecret, "the operator's static token")

	status, settings := statusOf(t, op, srv.URL+"/settings")
	if status != http.StatusOK {
		t.Fatalf("operator settings = %d", status)
	}
	for _, want := range []string{"Operator-provisioned", `data-static-token="1"`, `data-static-token="2"`, "CAIRN_API_TOKENS entry 2"} {
		if !strings.Contains(settings, want) {
			t.Errorf("operator Settings lacks %q", want)
		}
	}
	if _, other := statusOf(t, victim, srv.URL+"/settings"); strings.Contains(other, "data-static-token") {
		t.Error("a non-operator's Settings lists the operator's static tokens")
	}

	for _, page := range []string{settings, logs.String()} {
		for _, secret := range []string{opTokenSecret, opAgentTokenSecret} {
			if strings.Contains(page, secret) {
				t.Fatal("a token secret reached a page or a log line")
			}
		}
	}
}

// A static token stops authenticating the moment its user is suspended,
// because every request re-reads the user (SPEC-0023 "Suspending a user
// offboards them"). The operator console cannot suspend a listed operator,
// so this sets the column directly.
func TestIntegrationStaticTokenOfSuspendedUserRefused(t *testing.T) {
	idp := newFakeIdP(t)
	srv, api, pool := tokenOperatorServer(t, idp, io.Discard)
	signInAs(t, srv, idp, opSubject, opEmail, nil)
	if err := api.ResolveAPITokens(context.Background()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, status := whoamiAs(t, srv, opAgentTokenSecret); status != http.StatusOK {
		t.Fatalf("token whoami = %d before suspension, want 200", status)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET suspended_at = now() WHERE primary_email = $1`, opEmail); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, status := whoamiAs(t, srv, opAgentTokenSecret); status == http.StatusOK {
		t.Fatal("a suspended user's static token still authenticates")
	}
	err := api.ResolveAPITokens(context.Background())
	if err == nil || err.Error() != "CAIRN_API_TOKENS entry 1: "+errTokenSuspended.Error() {
		t.Fatalf("resolve for a suspended user = %v, want entry 1: %v", err, errTokenSuspended)
	}
}

// SPEC-0023 "Token naming a non-operator" and the other boot refusals:
// every entry that does not resolve to an existing, unsuspended operator
// fails with its position, and no error carries a secret.
func TestIntegrationResolveAPITokensRefusesNonOperators(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := user.NewStore(pool)
	mk := func(sub, email string, verified bool) {
		t.Helper()
		if _, err := users.Resolve(ctx, user.Identity{Issuer: testTokenIssuer, Subject: sub, Email: email, EmailVerified: verified}); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	mk("op-sub", "op@example.com", true)
	mk("member-sub", "member@example.com", true)
	mk("unverified-sub", "unverified@example.com", false)
	set, err := operator.Parse(testTokenIssuer+"|op-sub", "")
	if err != nil {
		t.Fatalf("operator.Parse: %v", err)
	}
	ops := operator.NewService(pool, set)

	const opEntry = "sk_boot_operator_0a1b2c3d4e5f:op@example.com"
	for _, tc := range []struct {
		name, second string
		want         error
	}{
		{"non-operator by email", "sk_boot_member_9f8e7d6c5b4a:member@example.com", errTokenNotOperator},
		{"non-operator by identity", "sk_boot_member_9f8e7d6c5b4a:" + testTokenIssuer + "|member-sub:human", errTokenNotOperator},
		{"unknown email", "sk_boot_nobody_1a2b3c4d5e6f:nobody@example.com", errTokenNoUser},
		{"unverified email", "sk_boot_unverif_6f5e4d3c2b1a:unverified@example.com", errTokenNoUser},
		{"unknown identity", "sk_boot_ghost_0f1e2d3c4b5a:" + testTokenIssuer + "|ghost", errTokenNoUser},
		{"operator's subject at another issuer", "sk_boot_elsewhere_a1b2c3d4:https://evil.example.com|op-sub", errTokenNoUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, err := ParseAPITokens(opEntry + "," + tc.second)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := resolveAPITokens(ctx, users, ops, tokens)
			if !errors.Is(err, tc.want) || got != nil {
				t.Fatalf("resolve = %v, %v; want %v", got, err, tc.want)
			}
			if want := "CAIRN_API_TOKENS entry 2: " + tc.want.Error(); err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
			for _, tok := range tokens {
				if strings.Contains(err.Error(), tok.Secret) {
					t.Fatal("the boot error carries a token secret")
				}
			}
		})
	}

	t.Run("operator resolves", func(t *testing.T) {
		tokens, err := ParseAPITokens(opEntry + ",sk_boot_again_77aa88bb99cc:" + testTokenIssuer + "|op-sub:human")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := resolveAPITokens(ctx, users, ops, tokens)
		if err != nil || len(got) != 2 {
			t.Fatalf("resolve = %v, %v; want both entries", got, err)
		}
		if got[0].UserID == "" || got[0].UserID != got[1].UserID {
			t.Fatalf("user ids = %q, %q; want the operator's, twice", got[0].UserID, got[1].UserID)
		}
		if !got[0].IsAgent || got[1].IsAgent {
			t.Fatalf("roles = agent:%v, agent:%v; want the default agent, then human", got[0].IsAgent, got[1].IsAgent)
		}
	})

	t.Run("no operator configured", func(t *testing.T) {
		tokens, err := ParseAPITokens(opEntry)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, err = resolveAPITokens(ctx, users, operator.NewService(pool, nil), tokens)
		if !errors.Is(err, errTokenNotOperator) {
			t.Fatalf("resolve with no operator = %v, want %v", err, errTokenNotOperator)
		}
	})
}
