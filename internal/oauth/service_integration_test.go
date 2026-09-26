package oauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/db"
)

var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) and
// applies the embedded migrations inside a private, per-test schema — the same
// isolation discipline as the other packages' integration suites.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run oauth integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("oauth_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// issueGrant walks a full code issue + redemption for tests, returning the set.
func issueGrant(t *testing.T, svc *Service, actor string) (*Client, *TokenSet, string) {
	t.Helper()
	ctx := context.Background()
	client, err := svc.RegisterClient(ctx, "Test Agent", []string{"https://client.example/cb"})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	verifier := strings.Repeat("v", 64)
	code, err := svc.CreateAuthCode(ctx, client.ID, actor, "https://client.example/cb", AllScopes(), S256Challenge(verifier))
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	set, err := svc.RedeemCode(ctx, code, client.ID, "https://client.example/cb", verifier)
	if err != nil {
		t.Fatalf("redeem code: %v", err)
	}
	return client, set, verifier
}

// TestIntegrationAudienceBinding proves an access token minted for one
// audience is rejected by a resource server validating a different audience
// (SPEC-0007 scenario "Access token bound to audience", RFC 8707).
func TestIntegrationAudienceBinding(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	cairn := NewService(pool, "https://cairn.test", Options{})
	other := NewService(pool, "https://other.example", Options{})

	_, set, _ := issueGrant(t, cairn, "sam@stump.rocks")

	ident, err := cairn.AuthenticateAccess(ctx, set.AccessToken)
	if err != nil {
		t.Fatalf("own-audience authenticate: %v", err)
	}
	if ident.ActorID != "sam@stump.rocks" {
		t.Fatalf("subject = %q, want the human", ident.ActorID)
	}
	if _, err := other.AuthenticateAccess(ctx, set.AccessToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("foreign-audience authenticate = %v, want ErrInvalidGrant", err)
	}
}

// TestIntegrationRefreshRotationAtomicity proves rotation invalidates the old
// refresh token and issues exactly one new pair, and that reusing the
// rotated-out token revokes the whole family (SPEC-0007 scenarios "Atomic
// refresh rotation", "Refresh rotation and reuse detection").
func TestIntegrationRefreshRotationAndReuseRevokesFamily(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://cairn.test", Options{})

	client, set, _ := issueGrant(t, svc, "sam@stump.rocks")

	rotated, err := svc.Refresh(ctx, set.RefreshToken, client.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rotated.RefreshToken == set.RefreshToken {
		t.Fatal("refresh did not rotate the token")
	}
	if rotated.GrantID != set.GrantID {
		t.Fatalf("rotation changed the grant: %q → %q", set.GrantID, rotated.GrantID)
	}
	// Exactly one live (unrotated, unrevoked) refresh token exists.
	var live int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM oauth_tokens
		WHERE grant_id = $1 AND kind = 'refresh' AND rotated_at IS NULL AND revoked_at IS NULL`,
		set.GrantID,
	).Scan(&live); err != nil {
		t.Fatalf("count refresh tokens: %v", err)
	}
	if live != 1 {
		t.Fatalf("live refresh tokens = %d, want exactly 1", live)
	}

	// Reusing the rotated-out token is theft: the family dies.
	if _, err := svc.Refresh(ctx, set.RefreshToken, client.ID); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("reuse = %v, want ErrInvalidGrant", err)
	}
	if _, err := svc.AuthenticateAccess(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("post-reuse access = %v, want ErrInvalidGrant (family revoked)", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken, client.ID); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("post-reuse refresh = %v, want ErrInvalidGrant (family revoked)", err)
	}
}

// TestIntegrationRevokeIsolation proves revoking one grant leaves a sibling
// grant for the same human untouched (SPEC-0007 scenario "Revoke one
// connection").
func TestIntegrationRevokeIsolation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://cairn.test", Options{})

	clientA, setA, _ := issueGrant(t, svc, "sam@stump.rocks")
	_, setB, _ := issueGrant(t, svc, "sam@stump.rocks")

	if err := svc.Revoke(ctx, setA.RefreshToken, clientA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.AuthenticateAccess(ctx, setA.AccessToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("revoked grant's access token = %v, want ErrInvalidGrant", err)
	}
	if _, err := svc.Refresh(ctx, setA.RefreshToken, clientA.ID); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("revoked grant's refresh token = %v, want ErrInvalidGrant", err)
	}
	if _, err := svc.AuthenticateAccess(ctx, setB.AccessToken); err != nil {
		t.Fatalf("sibling grant's access token = %v, want live", err)
	}
}

// TestIntegrationRevokeUnknownAndForeignTokens proves RFC 7009 semantics: an
// unknown token and another client's token both return success and revoke
// nothing.
func TestIntegrationRevokeUnknownAndForeignTokens(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://cairn.test", Options{})

	clientA, setA, _ := issueGrant(t, svc, "sam@stump.rocks")
	clientB, _, _ := issueGrant(t, svc, "sam@stump.rocks")

	if err := svc.Revoke(ctx, "cairn_rt_unknown", clientA.ID); err != nil {
		t.Fatalf("revoke unknown token: %v, want nil per RFC 7009", err)
	}
	// Client B presenting client A's token: treated as unknown, revokes nothing.
	if err := svc.Revoke(ctx, setA.RefreshToken, clientB.ID); err != nil {
		t.Fatalf("revoke foreign token: %v, want nil", err)
	}
	if _, err := svc.AuthenticateAccess(ctx, setA.AccessToken); err != nil {
		t.Fatalf("grant A access token = %v, want still live after foreign revoke", err)
	}
}

// TestIntegrationCodeBurnsOnFailedExchange proves a failed PKCE attempt spends
// the single-use code: the correct verifier no longer redeems it afterwards
// (SPEC-0007 REQ "OAuth 2.1 Authorization-Code + PKCE": codes are single-use).
func TestIntegrationCodeBurnsOnFailedExchange(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://cairn.test", Options{})

	client, err := svc.RegisterClient(ctx, "Test Agent", []string{"https://client.example/cb"})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	verifier := strings.Repeat("v", 64)
	code, err := svc.CreateAuthCode(ctx, client.ID, "sam@stump.rocks", "https://client.example/cb", AllScopes(), S256Challenge(verifier))
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	if _, err := svc.RedeemCode(ctx, code, client.ID, "https://client.example/cb", strings.Repeat("x", 64)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("bad verifier = %v, want ErrInvalidGrant", err)
	}
	if _, err := svc.RedeemCode(ctx, code, client.ID, "https://client.example/cb", verifier); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("burned code with good verifier = %v, want ErrInvalidGrant", err)
	}
}
