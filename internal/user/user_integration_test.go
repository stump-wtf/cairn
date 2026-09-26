package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/db"
)

var schemaSeq atomic.Int64

// newTestStore connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) in a
// private, migrated schema dropped on cleanup, matching the other packages.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run user integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("user_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
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
	return NewStore(pool), pool
}

const (
	pocketIssuer = "https://id.example.com"
	githubIssuer = "https://github.com"
)

func mustResolve(t *testing.T, s *Store, id Identity) *User {
	t.Helper()
	u, err := s.Resolve(context.Background(), id)
	if err != nil {
		t.Fatalf("Resolve(%+v): %v", id, err)
	}
	return u
}

// SPEC-0023 "Pocket ID and GitHub, same verified email": the second identity
// links to the first identity's user because both assert the same verified
// email, compared after lower-casing.
func TestResolveLinksSameVerifiedEmailAcrossProviders(t *testing.T) {
	s, _ := newTestStore(t)
	pocket := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "abc-123", Email: "joe@example.com", EmailVerified: true})
	gh := mustResolve(t, s, Identity{Issuer: githubIssuer, Subject: "583231", Email: "Joe@Example.com", EmailVerified: true, Handle: "octocat"})
	if gh.ID != pocket.ID {
		t.Fatalf("GitHub identity got user %s, want the Pocket ID user %s", gh.ID, pocket.ID)
	}
	if pocket.PrimaryEmail != "joe@example.com" || !pocket.EmailVerified {
		t.Errorf("primary email = %q verified=%v, want joe@example.com verified", pocket.PrimaryEmail, pocket.EmailVerified)
	}
}

// SPEC-0023 "Unverified email cannot claim a user": an identity presenting a
// victim's email unverified gets a user of its own with no primary email, and
// a later sign-in by the victim is not linked to it.
func TestResolveUnverifiedEmailCannotClaimUser(t *testing.T) {
	s, _ := newTestStore(t)
	victim := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "victim", Email: "victim@example.com", EmailVerified: true})
	attacker := mustResolve(t, s, Identity{Issuer: "https://evil.example.com", Subject: "attacker", Email: "victim@example.com", EmailVerified: false})
	if attacker.ID == victim.ID {
		t.Fatal("an unverified email claimed the victim's user")
	}
	if attacker.PrimaryEmail != "" || attacker.EmailVerified {
		t.Errorf("attacker primary email = %q verified=%v, want none", attacker.PrimaryEmail, attacker.EmailVerified)
	}

	// Order reversed: the unverified identity arrives first and must not
	// squat on the email for the verified owner who signs in later.
	s2, _ := newTestStore(t)
	squatter := mustResolve(t, s2, Identity{Issuer: "https://evil.example.com", Subject: "attacker", Email: "owner@example.com"})
	owner := mustResolve(t, s2, Identity{Issuer: pocketIssuer, Subject: "owner", Email: "owner@example.com", EmailVerified: true})
	if squatter.ID == owner.ID {
		t.Fatal("the verified owner was linked to a user created from an unverified email")
	}
	if owner.PrimaryEmail != "owner@example.com" {
		t.Errorf("owner primary email = %q, want owner@example.com", owner.PrimaryEmail)
	}
}

// A known identity keeps its user whatever email it presents later: linking
// happens once, at first sign-in, so an IdP that changes a user's email cannot
// move them onto someone else's user.
func TestResolveKnownIdentityKeepsItsUser(t *testing.T) {
	s, pool := newTestStore(t)
	a := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "a", Email: "a@example.com", EmailVerified: true})
	b := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "b", Email: "b@example.com", EmailVerified: true})
	again := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "a", Email: "b@example.com", EmailVerified: true})
	if again.ID != a.ID || again.ID == b.ID {
		t.Fatalf("identity a resolved to %s, want its own user %s", again.ID, a.ID)
	}
	var email string
	if err := pool.QueryRow(context.Background(),
		`SELECT email FROM user_identities WHERE issuer = $1 AND subject = 'a'`, pocketIssuer).Scan(&email); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if email != "b@example.com" {
		t.Errorf("identity email = %q, want the refreshed b@example.com", email)
	}
}

// Identities are keyed on (issuer, subject): the same subject under two
// issuers is two identities, and two users when no verified email links them.
func TestResolveKeysOnIssuerAndSubject(t *testing.T) {
	s, _ := newTestStore(t)
	one := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "same"})
	two := mustResolve(t, s, Identity{Issuer: githubIssuer, Subject: "same"})
	if one.ID == two.ID {
		t.Fatal("identities under different issuers shared a user without a verified email")
	}
}

// Concurrent first sign-ins with the same verified email converge on one user
// instead of failing on the unique index.
func TestResolveConcurrentFirstSignIns(t *testing.T) {
	s, _ := newTestStore(t)
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := s.Resolve(context.Background(), Identity{
				Issuer: pocketIssuer, Subject: fmt.Sprintf("sub-%d", i), Email: "race@example.com", EmailVerified: true,
			})
			errs[i] = err
			if u != nil {
				ids[i] = u.ID
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("sign-in %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("sign-in %d got user %s, want %s", i, ids[i], ids[0])
		}
	}
}

func TestDisplayHandles(t *testing.T) {
	s, _ := newTestStore(t)
	mustResolve(t, s, Identity{Issuer: githubIssuer, Subject: "1", Email: "sam@example.com", EmailVerified: true, Handle: "samdev"})
	got, err := s.DisplayHandles(context.Background(), []string{"Sam@Example.com", "nobody@example.com"})
	if err != nil {
		t.Fatalf("DisplayHandles: %v", err)
	}
	if got["sam@example.com"] != "samdev" {
		t.Errorf("handle = %q, want samdev", got["sam@example.com"])
	}
	if _, ok := got["nobody@example.com"]; ok {
		t.Error("an email with no user got a handle")
	}
}

func TestHandleFromEmail(t *testing.T) {
	for in, want := range map[string]string{
		"sam@example.com": "sam",
		"joe":             "joe",
		"":                "user",
		"@example.com":    "user",
	} {
		if got := HandleFromEmail(in); got != want {
			t.Errorf("HandleFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// FindByIdentity and FindByVerifiedEmail name an existing user and never
// create one: an unknown identity, an unknown email and an unverified email
// all answer ErrNotFound, and the lookups leave the users table untouched.
// They back the boot-time resolution of CAIRN_API_TOKENS (SPEC-0023 REQ
// "Static API Tokens Act as an Operator's User").
func TestFindExistingUserOnly(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	joe := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "joe-sub", Email: "joe@example.com", EmailVerified: true})
	sam := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sam-sub", Email: "sam@example.com"})

	u, err := s.FindByIdentity(ctx, pocketIssuer, "joe-sub")
	if err != nil || u.ID != joe.ID {
		t.Fatalf("FindByIdentity(joe) = %+v, %v; want %s", u, err, joe.ID)
	}
	u, err = s.FindByVerifiedEmail(ctx, "  Joe@Example.COM ")
	if err != nil || u.ID != joe.ID {
		t.Fatalf("FindByVerifiedEmail(joe) = %+v, %v; want %s", u, err, joe.ID)
	}
	if u, err := s.FindByIdentity(ctx, pocketIssuer, "sam-sub"); err != nil || u.ID != sam.ID {
		t.Fatalf("FindByIdentity(sam) = %+v, %v; want %s", u, err, sam.ID)
	}

	for name, find := range map[string]func() (*User, error){
		"unknown subject":   func() (*User, error) { return s.FindByIdentity(ctx, pocketIssuer, "nobody") },
		"subject elsewhere": func() (*User, error) { return s.FindByIdentity(ctx, githubIssuer, "joe-sub") },
		"blank identity":    func() (*User, error) { return s.FindByIdentity(ctx, "", "") },
		"unknown email":     func() (*User, error) { return s.FindByVerifiedEmail(ctx, "nobody@example.com") },
		"unverified email":  func() (*User, error) { return s.FindByVerifiedEmail(ctx, "sam@example.com") },
		"blank email":       func() (*User, error) { return s.FindByVerifiedEmail(ctx, " ") },
	} {
		if u, err := find(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: got %+v, %v; want ErrNotFound", name, u, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 2 {
		t.Fatalf("users = %d after lookups, want 2 (a lookup created a user)", n)
	}
}
