package event

import "testing"

// Governing: ADR-0022, SPEC-0016 EV-1 "Event Kind Registry".

// TestEventKindRegistry pins EV-1: the reserved and unknown kinds are not
// emittable, and every named kind is either registered or reserved.
func TestEventKindRegistry(t *testing.T) {
	for _, k := range []Kind{
		ArtifactCreated, CommentCreated, ReactionAdded,
		ReactionRemoved, RunClosed, ArtifactRetained,
		ArtifactReleased, ArtifactDeleted,
	} {
		if !k.Registered() || k.Reserved() {
			t.Errorf("%q: Registered=%v Reserved=%v, want registered and not reserved", k, k.Registered(), k.Reserved())
		}
	}
	for _, k := range []Kind{CommentEdited, CommentDeleted} {
		if k.Registered() || !k.Reserved() {
			t.Errorf("%q: Registered=%v Reserved=%v, want reserved and not registered", k, k.Registered(), k.Reserved())
		}
	}
	for _, k := range []Kind{"", "reaction.add", "comment.updated"} {
		if k.Registered() {
			t.Errorf("unknown kind %q is registered", k)
		}
	}
}

// TestActorKindValid pins that only the two derivable kinds are valid: an
// empty kind means nobody derived one, and a caller must not act on it.
func TestActorKindValid(t *testing.T) {
	for _, k := range []ActorKind{KindHuman, KindAgent} {
		if !k.Valid() {
			t.Errorf("%q.Valid() = false, want true", k)
		}
	}
	for _, k := range []ActorKind{"", "admin", "Human", "system"} {
		if k.Valid() {
			t.Errorf("%q.Valid() = true, want false", k)
		}
	}
}

// TestAuthMethodValid pins EV-4's closed set: the four stamped methods are
// valid, and an empty or invented method is not.
func TestAuthMethodValid(t *testing.T) {
	for _, m := range []AuthMethod{AuthSession, AuthOAuth, AuthPAT, AuthAPIToken} {
		if !m.Valid() {
			t.Errorf("%q.Valid() = false, want true", m)
		}
	}
	for _, m := range []AuthMethod{"", "cookie", "Session", "bearer", "api-token"} {
		if m.Valid() {
			t.Errorf("%q.Valid() = true, want false", m)
		}
	}
}

// TestActorCheck pins the fail-closed actor rule: id, kind and auth are all
// required, and the kind is human if and only if the auth is a session. The
// valid rows are the positive control for the refusals.
func TestActorCheck(t *testing.T) {
	cases := []struct {
		name string
		a    Actor
		ok   bool
	}{
		{"session human", Actor{ID: "u1", Kind: KindHuman, Auth: AuthSession}, true},
		{"pat agent", Actor{ID: "u1", Kind: KindAgent, Auth: AuthPAT}, true},
		{"oauth agent", Actor{ID: "u1", Kind: KindAgent, Auth: AuthOAuth}, true},
		{"api token agent", Actor{ID: "u1", Kind: KindAgent, Auth: AuthAPIToken}, true},
		{"no id", Actor{Kind: KindAgent, Auth: AuthPAT}, false},
		{"no kind", Actor{ID: "u1", Auth: AuthPAT}, false},
		{"no auth, agent", Actor{ID: "u1", Kind: KindAgent}, false},
		{"no auth, human", Actor{ID: "u1", Kind: KindHuman}, false},
		{"invented auth", Actor{ID: "u1", Kind: KindAgent, Auth: "cookie"}, false},
		{"human with a bearer", Actor{ID: "u1", Kind: KindHuman, Auth: AuthPAT}, false},
		{"human with oauth", Actor{ID: "u1", Kind: KindHuman, Auth: AuthOAuth}, false},
		{"agent with a session", Actor{ID: "u1", Kind: KindAgent, Auth: AuthSession}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.a.Check(); (err == nil) != tc.ok {
				t.Errorf("Check(%+v) = %v, want ok=%v", tc.a, err, tc.ok)
			}
		})
	}
}
