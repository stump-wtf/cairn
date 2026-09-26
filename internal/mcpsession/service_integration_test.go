package mcpsession

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
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/oauth"
)

var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) and
// applies the embedded migrations inside a private, per-test schema — the
// same isolation discipline internal/pat's and internal/oauth's integration
// suites use.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run mcpsession integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("mcpsession_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

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

// seedGrant walks a real OAuth authorization-code + PKCE redemption
// (mirroring internal/oauth's own issueGrant test helper) so mcp_sessions'
// grant_id foreign key has a genuine oauth_grants row to reference — a
// session is never recorded against a grant that doesn't exist.
func seedGrant(t *testing.T, pool *pgxpool.Pool, actor string) (clientID, grantID string) {
	t.Helper()
	ctx := context.Background()
	oauthSvc := oauth.NewService(pool, "https://cairn.test", oauth.Options{})
	client, err := oauthSvc.RegisterClient(ctx, "Test Agent", []string{"https://client.example/cb"})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	verifier := strings.Repeat("v", 64)
	code, err := oauthSvc.CreateAuthCode(ctx, client.ID, actor, "https://client.example/cb", oauth.AllScopes(), oauth.S256Challenge(verifier))
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	set, err := oauthSvc.RedeemCode(ctx, code, client.ID, "https://client.example/cb", verifier)
	if err != nil {
		t.Fatalf("redeem code: %v", err)
	}
	return client.ID, set.GrantID
}

// TestIntegrationRecordTouchList proves the headline round trip (issue #76:
// "a session row created/incremented by initialize + tool calls"): Record
// opens a session tied to its grant, Touch increments both the always-on
// tool-call count and the create/annotate-specific counters, and List
// surfaces the accumulated state, most recently active first.
func TestIntegrationRecordTouchList(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	clientID, grantID := seedGrant(t, pool, "sam@stump.rocks")
	svc := NewService(pool)

	sess, err := svc.Record(ctx, RecordInput{
		ID: "sess-1", OwnerID: "sam@stump.rocks", GrantID: grantID, ClientID: clientID,
		ClientName: "claude-code", ClientVersion: "1.2.3",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if sess.ClientName != "claude-code" || sess.ClientVersion != "1.2.3" {
		t.Fatalf("session client identity = %+v, want claude-code/1.2.3", sess)
	}

	if err := svc.Touch(ctx, "sess-1", ActivityToolCall); err != nil {
		t.Fatalf("touch tool call: %v", err)
	}
	if err := svc.Touch(ctx, "sess-1", ActivityArtifactCreated); err != nil {
		t.Fatalf("touch artifact created: %v", err)
	}
	if err := svc.Touch(ctx, "sess-1", ActivityAnnotationPosted); err != nil {
		t.Fatalf("touch annotation posted: %v", err)
	}

	list, err := svc.List(ctx, "sam@stump.rocks", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	got := list[0]
	if got.ToolCalls != 3 {
		t.Fatalf("tool_calls = %d, want 3 (every Touch counts a call)", got.ToolCalls)
	}
	if got.ArtifactsCreated != 1 {
		t.Fatalf("artifacts_created = %d, want 1", got.ArtifactsCreated)
	}
	if got.AnnotationsPosted != 1 {
		t.Fatalf("annotations_posted = %d, want 1", got.AnnotationsPosted)
	}
	if got.Ended() {
		t.Fatal("a session on a live grant must not report Ended")
	}
	if !got.LastActivityAt.After(got.ConnectedAt) && !got.LastActivityAt.Equal(got.ConnectedAt) {
		t.Fatalf("last_activity_at (%v) should be at or after connected_at (%v)", got.LastActivityAt, got.ConnectedAt)
	}
}

// TestIntegrationOwnerIsolation proves one owner's session list never
// surfaces another owner's sessions, and Get is owner-scoped-uniform-404 —
// the same discipline pat.Service and oauth.Service's grant isolation use
// (SPEC-0007 acceptance: "owner isolation").
func TestIntegrationOwnerIsolation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	_, grantA := seedGrant(t, pool, "alice@stump.rocks")
	clientB, grantB := seedGrant(t, pool, "bob@stump.rocks")
	svc := NewService(pool)

	if _, err := svc.Record(ctx, RecordInput{ID: "sess-a", OwnerID: "alice@stump.rocks", GrantID: grantA, ClientID: "client-a"}); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	if _, err := svc.Record(ctx, RecordInput{ID: "sess-b", OwnerID: "bob@stump.rocks", GrantID: grantB, ClientID: clientB}); err != nil {
		t.Fatalf("record bob: %v", err)
	}

	aliceList, err := svc.List(ctx, "alice@stump.rocks", 0)
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(aliceList) != 1 || aliceList[0].ID != "sess-a" {
		t.Fatalf("alice's list = %+v, want exactly her own session", aliceList)
	}

	// Bob cannot Get Alice's session by id.
	if _, err := svc.Get(ctx, "bob@stump.rocks", "sess-a"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-owner get = %v, want errs.ErrNotFound", err)
	}
	// Bob's own session is still reachable.
	if _, err := svc.Get(ctx, "bob@stump.rocks", "sess-b"); err != nil {
		t.Fatalf("bob's own get: %v", err)
	}
}

// TestIntegrationEndedReflectsRevokedGrant proves a session's Ended() flips
// true once its tied OAuth grant is revoked — "tie to the OAuth grant so
// revoking the grant ends the session" (issue #76) — with no separate
// revoked flag of this package's own to fall out of sync.
func TestIntegrationEndedReflectsRevokedGrant(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	clientID, grantID := seedGrant(t, pool, "sam@stump.rocks")
	svc := NewService(pool)
	oauthSvc := oauth.NewService(pool, "https://cairn.test", oauth.Options{})

	if _, err := svc.Record(ctx, RecordInput{ID: "sess-1", OwnerID: "sam@stump.rocks", GrantID: grantID, ClientID: clientID}); err != nil {
		t.Fatalf("record: %v", err)
	}
	before, err := svc.Get(ctx, "sam@stump.rocks", "sess-1")
	if err != nil {
		t.Fatalf("get before revoke: %v", err)
	}
	if before.Ended() {
		t.Fatal("session should not be ended before the grant is revoked")
	}

	if err := oauthSvc.RevokeGrant(ctx, grantID); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}

	after, err := svc.Get(ctx, "sam@stump.rocks", "sess-1")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if !after.Ended() {
		t.Fatal("session should report Ended once its grant is revoked")
	}
}

// TestIntegrationTouchUnknownSessionIsNoop proves Touch never fails a tool
// call over an unrecorded session id (e.g. a session this process never saw
// `initialize` for, after a restart) — activity counters are a best-effort
// presentation aid, not a correctness-critical ledger.
func TestIntegrationTouchUnknownSessionIsNoop(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	if err := svc.Touch(ctx, "sess-does-not-exist", ActivityToolCall); err != nil {
		t.Fatalf("touch unknown session should be a silent no-op, got: %v", err)
	}
}

// TestIntegrationGetUnknownIsNotFound proves Get on a never-recorded id is
// the same uniform not-found as a cross-owner lookup.
func TestIntegrationGetUnknownIsNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	if _, err := svc.Get(ctx, "sam@stump.rocks", "sess-does-not-exist"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("get unknown id = %v, want errs.ErrNotFound", err)
	}
}
