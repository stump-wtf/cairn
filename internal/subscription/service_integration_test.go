package subscription

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/user"
)

// Governing: SPEC-0023 REQ "Owned Outbound Subscriptions", REQ "Events Go
// Only to the Artifact's Workspace"

var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) in a
// private, migrated schema dropped on cleanup, matching the other packages.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run subscription integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("subscription_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
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

// testService is a Service whose policy resolves every *.example.com host to
// a public address without touching the network.
func testService(t *testing.T, pool *pgxpool.Pool, opts Options) *Service {
	t.Helper()
	if opts.Sealer == nil {
		opts.Sealer, _ = ParseKey(testKey(9))
	}
	if opts.Policy == nil {
		res := &switchResolver{}
		for _, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
			res.set(h, "93.184.216.34")
		}
		opts.Policy = &Policy{Resolver: res}
	}
	return NewService(pool, opts)
}

func newUser(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	u, err := user.NewStore(pool).ResolveActor(context.Background(), key)
	if err != nil {
		t.Fatalf("ResolveActor(%s): %v", key, err)
	}
	return u.ID
}

func mustCreate(t *testing.T, s *Service, in CreateInput) (string, *Subscription) {
	t.Helper()
	secret, sub, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return secret, sub
}

func TestServiceLifecycleIsOwnerScoped(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	ctx := context.Background()
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	v := Owner{UserID: newUser(t, pool, "v@example.com")}

	secret, sub := mustCreate(t, s, CreateInput{Owner: u, CreatedBy: u.UserID, URL: "https://a.example.com/webhooks/w/tok",
		EventTypes: []string{"artifact.created", "artifact.created"}, ShareTypes: []string{"markdown"}, Tags: []string{"handoff"}})
	if !strings.HasPrefix(secret, mintedPrefix) {
		t.Fatalf("minted secret %q lacks the %s prefix", secret, mintedPrefix)
	}
	if sub.Owner != u || !sub.Active() || len(sub.EventTypes) != 1 || sub.ShareTypes[0] != "markdown" || sub.Tags[0] != "handoff" {
		t.Fatalf("created %+v", sub)
	}

	// Stored encrypted: the plaintext is nowhere in the row.
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM outbound_subscriptions WHERE id = $1`, sub.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) || bytes.Contains(sealed, []byte(secret[len(mintedPrefix):])) {
		t.Fatal("the secret is stored in plaintext")
	}

	// The second user gets nothing: not in a list, and a uniform 404 on
	// every id-addressed call.
	if list, err := s.List(ctx, v); err != nil || len(list) != 0 {
		t.Fatalf("V's list = %v, %v; want empty", list, err)
	}
	if _, err := s.Get(ctx, v, sub.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("V Get = %v, want not found", err)
	}
	if _, err := s.SetPaused(ctx, v, sub.ID, true); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("V SetPaused = %v, want not found", err)
	}
	if _, _, err := s.Rotate(ctx, v, sub.ID, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("V Rotate = %v, want not found", err)
	}
	if err := s.Delete(ctx, v, sub.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("V Delete = %v, want not found", err)
	}
	if targets, _, _ := s.Targets(ctx, v, Match{Kind: "artifact.created", ShareType: "markdown", Tags: []string{"handoff"}}); len(targets) != 0 {
		t.Fatalf("V's workspace resolved U's subscription as a target: %v", targets)
	}
	if _, err := s.Get(ctx, u, "not-a-uuid"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get(malformed id) = %v, want not found", err)
	}

	// U pauses, resumes, rotates (to a receiver-issued secret) and deletes.
	if p, err := s.SetPaused(ctx, u, sub.ID, true); err != nil || p.Active() {
		t.Fatalf("pause = %+v, %v", p, err)
	}
	if p, err := s.SetPaused(ctx, u, sub.ID, false); err != nil || !p.Active() {
		t.Fatalf("resume = %+v, %v", p, err)
	}
	issued := "receiver-issued-" + strings.Repeat("k", 32)
	got, _, err := s.Rotate(ctx, u, sub.ID, issued)
	if err != nil || got != issued {
		t.Fatalf("rotate = %q, %v", got, err)
	}
	targets, _, err := s.Targets(ctx, u, Match{Kind: "artifact.created", ShareType: "markdown", Tags: []string{"x", "handoff"}})
	if err != nil || len(targets) != 1 || string(targets[0].Secret) != issued {
		t.Fatalf("targets after rotate = %v, %v", targets, err)
	}
	if err := s.Delete(ctx, u, sub.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := s.List(ctx, u); len(list) != 0 {
		t.Fatalf("list after delete = %v", list)
	}
}

func TestServiceRefusesBadInput(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	ctx := context.Background()
	for name, in := range map[string]CreateInput{
		"unknown event type": {Owner: u, URL: "https://a.example.com/h", EventTypes: []string{"artifact.exploded"}},
		"reserved kind":      {Owner: u, URL: "https://a.example.com/h", EventTypes: []string{"comment.edited"}},
		"unknown share type": {Owner: u, URL: "https://a.example.com/h", ShareTypes: []string{"hologram"}},
		"bad tag":            {Owner: u, URL: "https://a.example.com/h", Tags: []string{"UPPER"}},
		"short secret":       {Owner: u, URL: "https://a.example.com/h", Secret: "too-short"},
		"http target":        {Owner: u, URL: "http://a.example.com/h"},
		"no owner":           {URL: "https://a.example.com/h"},
	} {
		if _, _, err := s.Create(ctx, in); errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("%s: Create = %v, want a validation error", name, err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbound_subscriptions`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows created by refused requests", n)
	}

	noKey := NewService(pool, Options{Policy: s.policy})
	if _, _, err := noKey.Create(ctx, CreateInput{Owner: u, URL: "https://a.example.com/h"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Create without a key = %v, want ErrUnavailable", err)
	}
}

// TestCeilingHoldsUnderConcurrency: at most PerUser subscriptions per user,
// even when creates race.
func TestCeilingHoldsUnderConcurrency(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{PerUser: 5})
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	var ok, ceiling atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Create(context.Background(), CreateInput{Owner: u, URL: "https://a.example.com/h"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrCeiling):
				ceiling.Add(1)
			default:
				t.Errorf("Create: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 || ceiling.Load() != 7 {
		t.Fatalf("created %d, refused %d; want 5 and 7", ok.Load(), ceiling.Load())
	}
	// Another user's ceiling is their own.
	v := Owner{UserID: newUser(t, pool, "v@example.com")}
	mustCreate(t, s, CreateInput{Owner: v, URL: "https://a.example.com/h"})
	// A team's ceiling is PerTeam.
	if s.Ceiling(Owner{TeamID: "33333333-3333-4333-8333-333333333333"}) != DefaultPerTeam {
		t.Fatal("team ceiling is not the default 10")
	}
}

func TestTargetsFilters(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	ctx := context.Background()
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	_, all := mustCreate(t, s, CreateInput{Owner: u, URL: "https://a.example.com/all"})
	_, handoff := mustCreate(t, s, CreateInput{Owner: u, URL: "https://b.example.com/handoff", Tags: []string{"handoff", "lane:m"}})
	_, md := mustCreate(t, s, CreateInput{Owner: u, URL: "https://c.example.com/md", ShareTypes: []string{"markdown"}, EventTypes: []string{"artifact.created"}})
	_, reactions := mustCreate(t, s, CreateInput{Owner: u, URL: "https://c.example.com/r", EventTypes: []string{"reaction.added"}})

	ids := func(m Match) []string {
		t.Helper()
		targets, skipped, err := s.Targets(ctx, u, m)
		if err != nil || skipped != 0 {
			t.Fatalf("Targets = %v, %d, %v", targets, skipped, err)
		}
		var out []string
		for _, tg := range targets {
			out = append(out, tg.ID)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("targets = %v, want %v", got, want)
		}
	}
	eq(ids(Match{Kind: "artifact.created", ShareType: "file"}), all.ID)
	eq(ids(Match{Kind: "artifact.created", ShareType: "markdown", Tags: []string{"lane:m"}}), all.ID, handoff.ID, md.ID)
	eq(ids(Match{Kind: "reaction.added", ShareType: "markdown"}), all.ID, reactions.ID)

	// Paused and disabled subscriptions receive nothing.
	if _, err := s.SetPaused(ctx, u, all.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbound_subscriptions SET disabled_reason = 'x' WHERE id = $1`, md.ID); err != nil {
		t.Fatal(err)
	}
	eq(ids(Match{Kind: "artifact.created", ShareType: "markdown", Tags: []string{"handoff"}}), handoff.ID)

	// A suspended owner receives nothing.
	if _, err := pool.Exec(ctx, `UPDATE users SET suspended_at = now() WHERE id = $1`, u.UserID); err != nil {
		t.Fatal(err)
	}
	eq(ids(Match{Kind: "artifact.created", ShareType: "markdown", Tags: []string{"handoff"}}))
}

// TestTeamOwnedSubscriptionsAreSeparate: the schema's team owner is its own
// workspace — a team subscription is not a member's, and vice versa.
func TestTeamOwnedSubscriptionsAreSeparate(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	ctx := context.Background()
	member := Owner{UserID: newUser(t, pool, "m@example.com")}
	team := Owner{TeamID: "33333333-3333-4333-8333-333333333333"}
	_, ts := mustCreate(t, s, CreateInput{Owner: team, CreatedBy: member.UserID, URL: "https://a.example.com/team"})
	_, ms := mustCreate(t, s, CreateInput{Owner: member, URL: "https://b.example.com/member"})
	m := Match{Kind: "artifact.created", ShareType: "file"}
	if got, _, _ := s.Targets(ctx, team, m); len(got) != 1 || got[0].ID != ts.ID {
		t.Fatalf("team targets = %v", got)
	}
	if got, _, _ := s.Targets(ctx, member, m); len(got) != 1 || got[0].ID != ms.ID {
		t.Fatalf("member targets = %v", got)
	}
	if list, _ := s.List(ctx, member); len(list) != 1 {
		t.Fatalf("member list includes the team's subscription: %v", list)
	}
}

func TestTargetsSkipSecretsThatDoNotOpen(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	ctx := context.Background()
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	mustCreate(t, s, CreateInput{Owner: u, URL: "https://a.example.com/h"})
	otherKey, _ := ParseKey(testKey(3))
	rekeyed := NewService(pool, Options{Sealer: otherKey, Policy: s.policy})
	targets, skipped, err := rekeyed.Targets(ctx, u, Match{Kind: "artifact.created"})
	if err != nil || len(targets) != 0 || skipped != 1 {
		t.Fatalf("Targets under another key = %v, %d, %v; want none, 1 skipped", targets, skipped, err)
	}
	noKey := NewService(pool, Options{Policy: s.policy})
	if targets, skipped, _ := noKey.Targets(ctx, u, Match{Kind: "artifact.created"}); len(targets) != 0 || skipped != 1 {
		t.Fatalf("Targets with no key = %v, %d", targets, skipped)
	}
}

// TestRecordDisablesAfterTwentyFailures: the 20th consecutive failed
// delivery disables the subscription and says so; a success resets the
// count; resuming re-enables it.
func TestRecordDisablesAfterTwentyFailures(t *testing.T) {
	pool := newTestPool(t)
	s := testService(t, pool, Options{})
	ctx := context.Background()
	u := Owner{UserID: newUser(t, pool, "u@example.com")}
	_, sub := mustCreate(t, s, CreateInput{Owner: u, URL: "https://a.example.com/h"})

	fail := Result{Status: 503, Reason: ReasonHTTPStatus}
	for i := 0; i < 10; i++ {
		if d, err := s.Record(ctx, sub.ID, fail); err != nil || d {
			t.Fatalf("failure %d: disabled=%v err=%v", i+1, d, err)
		}
	}
	if _, err := s.Record(ctx, sub.ID, Result{OK: true, Status: 202}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, u, sub.ID)
	if got.ConsecutiveFailures != 0 || *got.LastStatus != 202 || got.LastError != "" || got.LastAttemptAt == nil {
		t.Fatalf("after a success: %+v", got)
	}
	for i := 1; i <= DisableAfter; i++ {
		d, err := s.Record(ctx, sub.ID, Result{Reason: ReasonConnection})
		if err != nil {
			t.Fatal(err)
		}
		if d != (i == DisableAfter) {
			t.Fatalf("failure %d: disabled=%v", i, d)
		}
	}
	got, _ = s.Get(ctx, u, sub.ID)
	if got.Active() || got.DisabledReason != DisabledReason || got.LastStatus != nil || got.LastError != ReasonConnection {
		t.Fatalf("after %d failures: %+v", DisableAfter, got)
	}
	if targets, _, _ := s.Targets(ctx, u, Match{Kind: "artifact.created"}); len(targets) != 0 {
		t.Fatal("a disabled subscription is still a target")
	}
	// A further failure (a delivery already in flight) does not re-report.
	if d, _ := s.Record(ctx, sub.ID, Result{Reason: ReasonConnection}); d {
		t.Fatal("an already-disabled subscription was reported disabled again")
	}
	if got, _ = s.SetPaused(ctx, u, sub.ID, false); !got.Active() || got.ConsecutiveFailures != 0 {
		t.Fatalf("resume did not re-enable: %+v", got)
	}
	if d, err := s.Record(ctx, "33333333-3333-4333-8333-333333333333", fail); d || err != nil {
		t.Fatalf("Record of a deleted subscription = %v, %v", d, err)
	}
}

// TestPolicyPermitAddrIsTheOnlyLoopbackPath guards the test-only knob: the
// zero policy refuses loopback, the knob admits it.
func TestPolicyPermitAddrIsTheOnlyLoopbackPath(t *testing.T) {
	if err := (&Policy{AllowHTTP: true}).CheckURL(context.Background(), "http://127.0.0.1:9/h"); err == nil {
		t.Fatal("the zero policy admitted a loopback target")
	}
	p := &Policy{AllowHTTP: true, PermitAddr: func(a netip.Addr) bool { return a.IsLoopback() }}
	if err := p.CheckURL(context.Background(), "http://127.0.0.1:9/h"); err != nil {
		t.Fatalf("the permit did not admit loopback: %v", err)
	}
}
