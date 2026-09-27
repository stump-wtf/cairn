package user

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Governing: ADR-0029, SPEC-0023 REQ "Owner Model", REQ "Migration to
// Explicit Ownership".

func mustResolveActor(t *testing.T, s *Store, key string) *User {
	t.Helper()
	u, err := s.ResolveActor(context.Background(), key)
	if err != nil {
		t.Fatalf("ResolveActor(%q): %v", key, err)
	}
	return u
}

// A string-keyed credential resolves to one user per key, renders as that
// key, and reaches a verified user only through that user's exact email.
func TestResolveActor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	bot := mustResolveActor(t, s, "ci-bot")
	if bot.Actor != "ci-bot" || bot.ActorKey != "ci-bot" || bot.EmailVerified || bot.PrimaryEmail != "" {
		t.Fatalf("new actor user = %+v, want an unverified user keyed and rendered ci-bot", bot)
	}
	if again := mustResolveActor(t, s, "ci-bot"); again.ID != bot.ID {
		t.Fatalf("ci-bot resolved to %s then %s", bot.ID, again.ID)
	}

	joe := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "joe", Email: "joe@example.com", EmailVerified: true})
	if got := mustResolveActor(t, s, "joe@example.com"); got.ID != joe.ID {
		t.Fatalf("joe@example.com resolved to %s, want the verified user %s", got.ID, joe.ID)
	}
	if got := mustResolveActor(t, s, "Joe@Example.com"); got.ID == joe.ID {
		t.Fatal("a different-case string reached the verified user: it was a different owner before")
	}

	anon := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "victim@example.com"})
	if anon.Actor != "user:"+anon.ID {
		t.Fatalf("email-shaped-subject user renders as %q, want its id", anon.Actor)
	}
	if got := mustResolveActor(t, s, "user:"+anon.ID); got.ID != anon.ID {
		t.Fatalf("user:<id> resolved to %s, want %s", got.ID, anon.ID)
	}
	if _, err := s.ResolveActor(ctx, "user:not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user:not-a-uuid = %v, want ErrNotFound", err)
	}
	if _, err := s.ResolveActor(ctx, "  "); err == nil {
		t.Fatal("a blank actor resolved")
	}
}

// Concurrent first resolutions of one key converge on one user.
func TestResolveActorConcurrent(t *testing.T) {
	s, _ := newTestStore(t)
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := s.ResolveActor(context.Background(), "race-bot")
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				return
			}
			ids[i] = u.ID
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if ids[i] != ids[0] {
			t.Fatalf("resolve %d got %s, want %s", i, ids[i], ids[0])
		}
	}
}

// A new user without a verified email renders as its subject when the subject
// may name it and nobody else holds that key; otherwise as its id.
func TestResolveSubjectActorKey(t *testing.T) {
	s, _ := newTestStore(t)
	first := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "abc-123"})
	if first.Actor != "abc-123" {
		t.Fatalf("actor = %q, want the subject", first.Actor)
	}
	second := mustResolve(t, s, Identity{Issuer: githubIssuer, Subject: "abc-123"})
	if second.ID == first.ID || second.Actor == "abc-123" {
		t.Fatalf("a second issuer's identity took the first user's key: %+v", second)
	}
	imitator := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "user:" + first.ID})
	if imitator.Actor == first.Actor || imitator.Actor == "user:"+first.ID {
		t.Fatalf("a user: subject imitates another user: renders %q", imitator.Actor)
	}
}

// SPEC-0023 "Existing owner signs in": a legacy owner string is claimed by the
// first sign-in whose verified email equals it, and never by an unverified one.
func TestResolveClaimsLegacyOwner(t *testing.T) {
	s, _ := newTestStore(t)
	legacy := mustResolveActor(t, s, "sam@example.com")

	unverified := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "mallory", Email: "sam@example.com"})
	if unverified.ID == legacy.ID {
		t.Fatal("an unverified email claimed the legacy owner")
	}

	sam := mustResolve(t, s, Identity{Issuer: githubIssuer, Subject: "42", Email: "SAM@example.com", EmailVerified: true})
	if sam.ID != legacy.ID {
		t.Fatalf("verified sign-in got user %s, want the legacy user %s", sam.ID, legacy.ID)
	}
	if !sam.EmailVerified || sam.PrimaryEmail != "sam@example.com" || sam.Actor != "sam@example.com" {
		t.Fatalf("claimed user = %+v, want verified sam@example.com", sam)
	}
	// A second provider with the same verified email links to the same user.
	if again := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sam", Email: "sam@example.com", EmailVerified: true}); again.ID != legacy.ID {
		t.Fatalf("second verified identity got %s, want %s", again.ID, legacy.ID)
	}
}

// The #384 review's probe, inverted: a legacy owner is claimed by a LATER
// verified sign-in of an identity whose first sign-in was unverified. Against
// the one-shot resolver the second sign-in kept the subject-keyed user from
// the first, so the legacy Bin was stranded for good.
func TestResolveLaterVerifiedSignInClaimsLegacyOwner(t *testing.T) {
	s, _ := newTestStore(t)
	legacy := mustResolveActor(t, s, "joe@stump.example")

	first := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-joe", Email: "joe@stump.example"})
	if first.ID == legacy.ID {
		t.Fatal("an unverified first sign-in claimed the legacy owner")
	}
	later := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-joe", Email: "joe@stump.example", EmailVerified: true})
	if later.ID != legacy.ID {
		t.Fatalf("verified second sign-in got %s, want the legacy user %s", later.ID, legacy.ID)
	}
	if !later.EmailVerified || later.PrimaryEmail != "joe@stump.example" {
		t.Fatalf("claimed user = %+v, want verified joe@stump.example", later)
	}
	// And it stays there.
	if again := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-joe", Email: "joe@stump.example"}); again.ID != legacy.ID {
		t.Fatalf("settled identity moved to %s", again.ID)
	}
}

// An identity already linked to a legacy owner (the ownership migration links
// one from its pre-users session, which main keyed on the raw email claim,
// case and all) claims that same user when its email is verified, even when
// another legacy user holds the exact lower-case string: its own user wins.
func TestResolveRematchPrefersOwnLegacyUser(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	variant := mustResolveActor(t, s, "kim@example.com")
	own := mustResolveActor(t, s, "Kim@Example.com")
	if _, err := pool.Exec(ctx, `INSERT INTO user_identities (user_id, issuer, subject, email_verified)
		VALUES ($1, $2, 'sub-kim', false)`, own.ID, pocketIssuer); err != nil {
		t.Fatalf("link identity: %v", err)
	}
	// Unverified: it is already on its own user, and nothing is claimed.
	if u := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-kim", Email: "kim@example.com"}); u.ID != own.ID || u.PrimaryEmail != "" {
		t.Fatalf("unverified sign-in = %+v, want its own unclaimed user %s", u, own.ID)
	}
	u := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-kim", Email: "kim@example.com", EmailVerified: true})
	if u.ID != own.ID || u.ID == variant.ID || u.PrimaryEmail != "kim@example.com" {
		t.Fatalf("verified sign-in = %+v, want its own user %s claimed", u, own.ID)
	}
}

// With nothing to match, a verified email is taken by the identity's own
// user rather than a new one, so whatever it owns stays in its Bin.
func TestResolveRematchVerifiesOwnUser(t *testing.T) {
	s, _ := newTestStore(t)
	first := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-ann", Email: "ann@example.com"})
	later := mustResolve(t, s, Identity{Issuer: pocketIssuer, Subject: "sub-ann", Email: "ann@example.com", EmailVerified: true})
	if later.ID != first.ID || later.PrimaryEmail != "ann@example.com" || !later.EmailVerified {
		t.Fatalf("verified sign-in = %+v, want user %s holding ann@example.com", later, first.ID)
	}
}
