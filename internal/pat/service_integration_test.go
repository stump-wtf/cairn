package pat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
)

var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) and
// applies the embedded migrations inside a private, per-test schema — the
// same isolation discipline internal/oauth's integration suite uses.
// sam, alice and bob are the users newTestPool seeds.
const (
	sam   = "00000000-0000-4000-8000-0000000000e1"
	alice = "00000000-0000-4000-8000-0000000000e2"
	bob   = "00000000-0000-4000-8000-0000000000e3"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run pat integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("pat_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

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
	// Tokens belong to users (SPEC-0023 REQ "Owner Model").
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, actor_key, display_handle) VALUES
		($1, 'sam@stump.rocks', 'sam'), ($2, 'alice@stump.rocks', 'alice'), ($3, 'bob@stump.rocks', 'bob')`,
		sam, alice, bob); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	return pool
}

// TestIntegrationCreateAuthenticateRevoke walks the full lifecycle: mint a
// token, authenticate with its plaintext secret, revoke it, and prove the
// revoked secret is rejected — the round trip issue #74 requires ("create →
// use as bearer → list → revoke → rejected").
func TestIntegrationCreateAuthenticateRevoke(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	secret, tok, err := svc.Create(ctx, sam, "laptop agent", []string{ScopeArtifactsRead, ScopeArtifactsWrite}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if secret == "" {
		t.Fatal("create returned an empty secret")
	}
	if tok.RevokedAt != nil {
		t.Fatal("freshly minted token must not be revoked")
	}

	got, err := svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.UserID != sam || got.Actor != "sam@stump.rocks" {
		t.Fatalf("owner = %q (%q), want sam@stump.rocks", got.UserID, got.Actor)
	}
	if !got.IsAgent {
		t.Fatal("is_agent flag did not round-trip")
	}
	if got.LastUsedAt == nil {
		t.Fatal("authenticate must set last_used_at")
	}
	if len(got.Scopes) != 2 {
		t.Fatalf("scopes = %v, want exactly the granted two", got.Scopes)
	}

	if err := svc.Revoke(ctx, sam, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Authenticate(ctx, secret); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("post-revoke authenticate = %v, want errs.ErrUnauthorized", err)
	}

	toks, err := svc.List(ctx, sam)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("list len = %d, want 1", len(toks))
	}
	if toks[0].RevokedAt == nil {
		t.Fatal("listed token should show revoked_at set")
	}
}

// TestIntegrationOwnerIsolation proves one owner's revoke cannot touch
// another owner's token, and List never crosses owners (issue #74
// acceptance: "owner isolation").
func TestIntegrationOwnerIsolation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	_, tokA, err := svc.Create(ctx, alice, "alice's token", AllScopes(), false)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	secretB, tokB, err := svc.Create(ctx, bob, "bob's token", AllScopes(), false)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Bob cannot revoke Alice's token by id.
	if err := svc.Revoke(ctx, bob, tokA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-owner revoke = %v, want errs.ErrNotFound", err)
	}
	// Bob's own token still authenticates — Alice's failed revoke attempt did
	// nothing to Bob's token, and Bob's token was never touched either.
	if _, err := svc.Authenticate(ctx, secretB); err != nil {
		t.Fatalf("bob's token should still authenticate: %v", err)
	}

	aliceList, err := svc.List(ctx, alice)
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(aliceList) != 1 || aliceList[0].ID != tokA.ID {
		t.Fatalf("alice's list = %+v, want exactly her own token", aliceList)
	}
	bobList, err := svc.List(ctx, bob)
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bobList) != 1 || bobList[0].ID != tokB.ID {
		t.Fatalf("bob's list = %+v, want exactly his own token", bobList)
	}
}

// TestIntegrationRevokeUnknownIsNotFound proves revoking an id that never
// existed is the same uniform not-found as revoking someone else's token —
// no existence oracle.
func TestIntegrationRevokeUnknownIsNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	if err := svc.Revoke(ctx, sam, "pat-does-not-exist"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke unknown id = %v, want errs.ErrNotFound", err)
	}
}

// TestIntegrationRevokeIsIdempotentFailure proves revoking an already-revoked
// token a second time also fails not-found (it is no longer live), rather
// than silently succeeding twice.
func TestIntegrationRevokeIsIdempotentFailure(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	_, tok, err := svc.Create(ctx, sam, "one-shot", AllScopes(), false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Revoke(ctx, sam, tok.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := svc.Revoke(ctx, sam, tok.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("second revoke = %v, want errs.ErrNotFound", err)
	}
}

// TestIntegrationAuthenticateRejectsUnknownSecret proves an unknown secret
// (never minted) fails closed.
func TestIntegrationAuthenticateRejectsUnknownSecret(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	if _, err := svc.Authenticate(ctx, "cairn_pat_totally-unknown"); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("unknown secret = %v, want errs.ErrUnauthorized", err)
	}
}

// TestIntegrationCreateRejectsEmptyScopes proves the service enforces the
// domain invariant regardless of what a caller passes (defense in depth
// behind the transport's own ParseScope validation).
func TestIntegrationCreateRejectsEmptyScopes(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	if _, _, err := svc.Create(ctx, sam, "empty", nil, false); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create with no scopes = %v, want errs.ErrValidation", err)
	}
}
