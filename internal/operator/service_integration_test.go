package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/pat"
	"github.com/stump-wtf/cairn/internal/session"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/user"
)

var schemaSeq atomic.Int64

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) in a
// private, migrated schema dropped on cleanup, matching the other packages.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run operator integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("operator_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
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

const (
	testIssuer   = "https://id.example.com"
	opSubject    = "op-subject"
	pkceVerifier = "operator-test-verifier-0123456789-abcdefghijklmnopqrstuvwxyz"
)

type fixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	svc   *Service
	users *user.Store
	op    *user.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := newTestPool(t)
	set, err := Parse(testIssuer+"|"+opSubject, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := &fixture{ctx: context.Background(), pool: pool, svc: NewService(pool, set), users: user.NewStore(pool)}
	f.op = f.signIn(t, opSubject, "op@example.com")
	return f
}

func (f *fixture) signIn(t *testing.T, subject, email string) *user.User {
	t.Helper()
	u, err := f.users.Resolve(f.ctx, user.Identity{Issuer: testIssuer, Subject: subject, Email: email, EmailVerified: true})
	if err != nil {
		t.Fatalf("Resolve %s: %v", subject, err)
	}
	return u
}

// credentials gives u one of everything suspension must end: a browser
// session, a personal access token, an OAuth grant with its access and
// refresh token, and an unredeemed authorization code.
type credentials struct {
	session             string
	pat                 string
	access, refresh     string
	pendingCode, client string
	sessions            *session.PostgresStore
	pats                *pat.Service
	oauth               *oauth.Service
}

func (f *fixture) credentialsFor(t *testing.T, u *user.User) *credentials {
	t.Helper()
	c := &credentials{
		sessions: session.NewPostgresStore(f.pool),
		pats:     pat.NewService(f.pool),
		oauth:    oauth.NewService(f.pool, "http://cairn.test", oauth.Options{}),
	}
	sess, err := c.sessions.Create(f.ctx, testIssuer, "s", u.Actor, u.ID, "", time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	c.session = sess.Token
	c.pat, _, err = c.pats.Create(f.ctx, u.ID, "laptop", []string{"artifacts:read"}, false)
	if err != nil {
		t.Fatalf("pat: %v", err)
	}
	client, err := c.oauth.RegisterClient(f.ctx, "agent", []string{"http://127.0.0.1:9/cb"})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	c.client = client.ID
	code, err := c.oauth.CreateAuthCode(f.ctx, client.ID, u.ID, "http://127.0.0.1:9/cb", []string{"artifacts:read"}, oauth.S256Challenge(pkceVerifier))
	if err != nil {
		t.Fatalf("auth code: %v", err)
	}
	set, err := c.oauth.RedeemCode(f.ctx, code, client.ID, "http://127.0.0.1:9/cb", pkceVerifier)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	c.access, c.refresh = set.AccessToken, set.RefreshToken
	c.pendingCode, err = c.oauth.CreateAuthCode(f.ctx, client.ID, u.ID, "http://127.0.0.1:9/cb", []string{"artifacts:read"}, oauth.S256Challenge(pkceVerifier))
	if err != nil {
		t.Fatalf("pending auth code: %v", err)
	}
	return c
}

// assertLive reports which of c still authenticates, failing the test for
// every one whose liveness differs from want.
func (c *credentials) assert(t *testing.T, ctx context.Context, want bool) {
	t.Helper()
	_, err := c.sessions.Get(ctx, c.session)
	if (err == nil) != want {
		t.Errorf("session live = %v, want %v (err %v)", err == nil, want, err)
	}
	_, err = c.pats.Authenticate(ctx, c.pat)
	if (err == nil) != want {
		t.Errorf("personal access token live = %v, want %v (err %v)", err == nil, want, err)
	}
	_, err = c.oauth.AuthenticateAccess(ctx, c.access)
	if (err == nil) != want {
		t.Errorf("oauth access token live = %v, want %v (err %v)", err == nil, want, err)
	}
}

// SPEC-0023 "Suspending a user offboards them": sessions, personal access
// tokens, OAuth grants and refresh tokens stop authenticating on the next
// request, and the audit row is written with the action.
func TestSetSuspendedOffboardsAndAudits(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	bystander := f.signIn(t, "b-subject", "b@example.com")
	creds := f.credentialsFor(t, u)
	other := f.credentialsFor(t, bystander)
	creds.assert(t, f.ctx, true)

	res, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: "  spam  "})
	if err != nil {
		t.Fatalf("SetSuspended: %v", err)
	}
	if res.SuspendedAt == nil {
		t.Fatal("result carries no suspended_at")
	}
	want := Revoked{Sessions: 1, PersonalTokens: 1, OAuthGrants: 1, OAuthTokens: 2, OAuthAuthorizations: 1}
	if res.Revoked != want {
		t.Errorf("revoked = %+v, want %+v", res.Revoked, want)
	}

	creds.assert(t, f.ctx, false)
	if _, err := creds.oauth.Refresh(f.ctx, creds.refresh, creds.client); err == nil {
		t.Error("refresh token still rotates after suspension")
	}
	if _, err := creds.oauth.RedeemCode(f.ctx, creds.pendingCode, creds.client, "http://127.0.0.1:9/cb", pkceVerifier); err == nil {
		t.Error("an authorization code minted before suspension still redeems")
	}
	// Someone else's credentials are untouched.
	other.assert(t, f.ctx, true)

	entries, err := f.svc.AuditFor(f.ctx, u.ID, 10)
	if err != nil {
		t.Fatalf("AuditFor: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit rows for U = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ID != res.AuditID || e.Action != ActionSuspendUser || e.Reason != "spam" || e.TargetUserID != u.ID {
		t.Errorf("audit row = %+v", e)
	}
	if e.OperatorHandle != f.op.DisplayHandle || strings.Contains(e.OperatorHandle, "@") {
		t.Errorf("audit operator = %q, want the display handle %q", e.OperatorHandle, f.op.DisplayHandle)
	}
	var detail struct{ Revoked Revoked }
	if err := json.Unmarshal(e.Detail, &detail); err != nil || detail.Revoked != want {
		t.Errorf("audit detail = %s (%v), want the revoked counts", e.Detail, err)
	}
	if byOther, _ := f.svc.AuditFor(f.ctx, bystander.ID, 10); len(byOther) != 0 {
		t.Errorf("a bystander reads %d audit rows about someone else", len(byOther))
	}
}

// Every credential check refuses a suspended user even when its credential
// was not revoked: the guard for a credential minted in the window between
// the revocation and the flag, or one the revocation missed.
func TestSuspendedUserFailsClosedWithoutRevocation(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	creds := f.credentialsFor(t, u)
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET suspended_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("flag: %v", err)
	}
	creds.assert(t, f.ctx, false)
	if _, err := creds.oauth.Refresh(f.ctx, creds.refresh, creds.client); err == nil {
		t.Error("refresh succeeded for a suspended user")
	}
	if _, err := creds.oauth.RedeemCode(f.ctx, creds.pendingCode, creds.client, "http://127.0.0.1:9/cb", pkceVerifier); err == nil {
		t.Error("code redeemed for a suspended user")
	}
}

func TestReinstateRestoresNothingAndAudits(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	creds := f.credentialsFor(t, u)
	if _, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: "spam"}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	res, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: false, Reason: "appeal upheld"})
	if err != nil {
		t.Fatalf("reinstate: %v", err)
	}
	if res.SuspendedAt != nil || res.Revoked != (Revoked{}) {
		t.Errorf("reinstate result = %+v", res)
	}
	got, err := f.users.Get(f.ctx, u.ID)
	if err != nil || got.SuspendedAt != nil {
		t.Fatalf("user after reinstate = %+v, %v", got, err)
	}
	// Revoked credentials stay revoked; the user signs in again.
	creds.assert(t, f.ctx, false)

	entries, err := f.svc.AuditFor(f.ctx, u.ID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit rows = %d (%v), want 2", len(entries), err)
	}
	if entries[0].Action != ActionUnsuspendUser || entries[0].Reason != "appeal upheld" || entries[1].Action != ActionSuspendUser {
		t.Errorf("audit order/content = %+v", entries)
	}
}

func TestSetSuspendedRefusals(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	long := strings.Repeat("x", MaxReasonRunes+1)
	for _, tc := range []struct {
		name string
		in   SuspensionInput
		want error
	}{
		{"no reason", SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: "   "}, ErrReason},
		{"reason too long", SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: long}, ErrReason},
		{"self", SuspensionInput{OperatorID: f.op.ID, UserID: f.op.ID, Suspend: true, Reason: "r"}, ErrSelf},
		{"unknown user", SuspensionInput{OperatorID: f.op.ID, UserID: "00000000-0000-0000-0000-000000000000", Suspend: true, Reason: "r"}, errs.ErrNotFound},
		{"malformed user", SuspensionInput{OperatorID: f.op.ID, UserID: "not-a-uuid", Suspend: true, Reason: "r"}, errs.ErrNotFound},
		{"not suspended", SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: false, Reason: "r"}, errs.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.SetSuspended(f.ctx, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM operator_audit`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused actions wrote %d audit rows (%v), want 0", n, err)
	}

	// A second operator cannot suspend a CAIRN_OPERATORS identity, and the
	// refusal leaves the target untouched.
	other := f.signIn(t, "other-op", "other@example.com")
	if _, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: other.ID, UserID: f.op.ID, Suspend: true, Reason: "r"}); !errors.Is(err, ErrProtected) {
		t.Fatalf("suspend listed operator: err = %v, want ErrProtected", err)
	}
	if got, _ := f.users.Get(f.ctx, f.op.ID); got.SuspendedAt != nil {
		t.Fatal("a refused suspension still set suspended_at")
	}

	if _, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: "r"}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := f.svc.SetSuspended(f.ctx, SuspensionInput{OperatorID: f.op.ID, UserID: u.ID, Suspend: true, Reason: "r"}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second suspend: err = %v, want conflict", err)
	}
}

// SPEC-0023 REQ "Operator Surfaces Bound Tenant Data and Never Read It": the
// directory carries counts, and nothing that identifies or reveals an
// artifact.
func TestDirectoryCountsWithoutContent(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	st := store.New(f.pool, objectstore.NewMemory(), store.Options{})
	var ids []string
	for i, body := range []string{"# secret plan", "second secret body"} {
		a, err := st.CreateArtifact(f.ctx, store.CreateArtifactInput{
			ShareType:  artifact.ShareType("markdown"),
			Title:      fmt.Sprintf("Private title %d", i),
			Body:       strings.NewReader(body),
			Provenance: artifact.Provenance{ActorID: u.Actor, CreatedByUserID: u.ID, Channel: artifact.ChannelAPI, CapturedAt: time.Now()},
			Access:     artifact.AccessPolicy{OwnerUserID: u.ID, Visibility: artifact.VisibilityPrivate},
			ExpiresAt:  time.Now().Add(time.Hour),
			Tags:       []string{"handoff"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, a.PublicID)
	}

	dir, err := f.svc.Directory(f.ctx, 10, 0)
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	byID := map[string]DirectoryUser{}
	for _, d := range dir {
		byID[d.ID] = d
	}
	got, ok := byID[u.ID]
	if !ok || got.Artifacts != 2 || got.Bytes != int64(len("# secret plan")+len("second secret body")) {
		t.Fatalf("U in directory = %+v (present %v), want 2 artifacts and their bytes", got, ok)
	}
	if !byID[f.op.ID].Operator || got.Operator {
		t.Errorf("operator flags: op=%v u=%v, want true false", byID[f.op.ID].Operator, got.Operator)
	}
	if got.LastSeenAt == nil || got.Actor != "u@example.com" {
		t.Errorf("identity fields = %+v", got)
	}
	raw, _ := json.Marshal(dir)
	for _, leak := range append(ids, "Private title", "secret", "handoff") {
		if strings.Contains(string(raw), leak) {
			t.Errorf("directory leaks %q", leak)
		}
	}

	page, err := f.svc.Directory(f.ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != dir[1].ID {
		t.Fatalf("paged directory = %+v (%v), want the second user", page, err)
	}
}

func TestIsOperatorUser(t *testing.T) {
	f := newFixture(t)
	u := f.signIn(t, "u-subject", "u@example.com")
	for _, tc := range []struct {
		id   string
		want bool
	}{{f.op.ID, true}, {u.ID, false}, {"nope", false}} {
		got, err := f.svc.IsOperatorUser(f.ctx, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("IsOperatorUser(%s) = %v, %v; want %v", tc.id, got, err, tc.want)
		}
	}
}
