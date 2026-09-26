package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/config"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/subscription"
	"github.com/stump-wtf/cairn/internal/user"
)

// Governing: SPEC-0023 REQ "Removing the Instance-Wide Outbound Targets"

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) in a
// private, migrated schema dropped on cleanup, matching the other packages.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run cairnd integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("cairnd_test_%d", time.Now().UnixNano())
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

// TestLeftoverVariableDeliversNothing is scenario "A leftover variable
// delivers nothing", through the same wiring run() uses: with the removed
// variables still set and pointing at a live receiver, cairnd's config loads,
// its emitter runs, and an artifact tagged handoff — the operator's and
// another user's — makes no request to that URL. The positive control is the
// operator's own subscription: its health shows exactly one attempt, so the
// events did flow through the delivery pipeline, to the owner's subscription
// only.
func TestLeftoverVariableDeliversNothing(t *testing.T) {
	var leftoverHits atomic.Int32
	leftover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leftoverHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer leftover.Close()

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	t.Setenv("CAIRN_OUTBOUND_"+"WEBHOOK_URLS", leftover.URL)
	t.Setenv("CAIRN_OUTBOUND_"+"WEBHOOK_SECRET", strings.Repeat("s", 40))
	t.Setenv("CAIRN_BASE_URL", "https://cairn.example.com")
	t.Setenv("CAIRN_ENCRYPTION_KEY", key)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load with the removed variables set: %v", err)
	}

	pool := newTestPool(t)
	ctx := context.Background()
	users := user.NewStore(pool)
	operatorUser, err := users.ResolveActor(ctx, "operator@example.com")
	if err != nil {
		t.Fatal(err)
	}
	otherUser, err := users.ResolveActor(ctx, "other@example.com")
	if err != nil {
		t.Fatal(err)
	}

	subs, err := newSubscriptions(cfg, pool, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	// The operator's replacement subscription. Created through a policy that
	// admits the loopback receiver; cairnd's own policy (the emitter's) then
	// refuses to dial it, which still records the attempt.
	sealer, _ := subscription.ParseKey(key)
	creator := subscription.NewService(pool, subscription.Options{
		Sealer: sealer,
		Policy: &subscription.Policy{PermitAddr: func(a netip.Addr) bool { return a.IsLoopback() }},
	})
	_, sub, err := creator.Create(ctx, subscription.CreateInput{
		Owner: subscription.Owner{UserID: operatorUser.ID}, CreatedBy: operatorUser.ID,
		URL: "https://127.0.0.1:9/replacement",
	})
	if err != nil {
		t.Fatal(err)
	}

	emitter := newOutboundEmitter(cfg, subs, quietLogger())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); emitter.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	st := store.New(pool, objectstore.NewMemory(), newStoreOptions(cfg, emitter))

	for _, u := range []*user.User{operatorUser, otherUser} {
		if _, err := st.CreateArtifact(ctx, store.CreateArtifactInput{
			ShareType:  artifact.TypeFile,
			Title:      "handoff",
			Body:       strings.NewReader("handoff body"),
			Provenance: artifact.Provenance{CreatedByUserID: u.ID, ActorID: u.ID, Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
			Access:     artifact.AccessPolicy{OwnerUserID: u.ID, Visibility: artifact.VisibilityLink},
			ExpiresAt:  time.Now().Add(time.Hour),
			Tags:       []string{"handoff"},
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	owner := subscription.Owner{UserID: operatorUser.ID}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := subs.Get(ctx, owner, sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastAttemptAt != nil {
			if got.ConsecutiveFailures != 1 || got.LastError != subscription.ReasonBlockedAddress {
				t.Fatalf("operator subscription health = %d failures, %q; want exactly one blocked attempt (the operator's artifact only)",
					got.ConsecutiveFailures, got.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the operator's artifact never reached the operator's subscription")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // give any (wrong) delivery a chance
	if n := leftoverHits.Load(); n != 0 {
		t.Fatalf("the removed variable's URL received %d requests", n)
	}
	got, _ := subs.Get(ctx, owner, sub.ID)
	if got.ConsecutiveFailures != 1 {
		t.Fatalf("the other user's artifact reached the operator's subscription (%d attempts)", got.ConsecutiveFailures)
	}
}
