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
