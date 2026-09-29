package annotation

import (
	"context"
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// TestWritesRequireDerivedActor pins the fail-closed half of SPEC-0016 EV-4 at
// the service boundary: every annotation write refuses an actor with no id, no
// derived kind, no auth method, or a kind that contradicts its auth method,
// before it opens a transaction. The service here has no pool, so a write that
// got past the check would panic rather than pass.
//
// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-3, EV-4.
func TestWritesRequireDerivedActor(t *testing.T) {
	svc := NewService(nil, nil)
	ctx := context.Background()

	writesFor := func(a event.Actor) map[string]func() error {
		return map[string]func() error{
			"React": func() error {
				_, _, err := svc.React(ctx, "SVCAAAA1", sharetype.AnchorArtifact, nil, "🔥", a)
				return err
			},
			"Unreact": func() error {
				_, err := svc.Unreact(ctx, "SVCAAAA1", sharetype.AnchorArtifact, nil, "🔥", a)
				return err
			},
			"UnreactByID": func() error {
				return svc.UnreactByID(ctx, "SVCAAAA1", 1, a)
			},
			"AddComment": func() error {
				_, err := svc.AddComment(ctx, "SVCAAAA1", CommentInput{
					AnchorType: sharetype.AnchorArtifact, Actor: a, Body: "hi",
				})
				return err
			},
			"EditComment": func() error {
				return svc.EditComment(ctx, "SVCAAAA1", 1, a, "hi")
			},
			"DeleteComment": func() error {
				return svc.DeleteComment(ctx, "SVCAAAA1", 1, a)
			},
		}
	}

	bad := map[string]event.Actor{
		"no id":         {Kind: event.KindAgent, Auth: event.AuthPAT},
		"no kind":       {ID: "u1", Auth: event.AuthPAT},
		"invented kind": {ID: "u1", Kind: event.ActorKind("admin"), Auth: event.AuthSession},
		// EV-3/EV-4: auth is required, from the closed set, and must agree with
		// the kind (human iff session).
		"no auth":            {ID: "u1", Kind: event.KindAgent},
		"no auth, human":     {ID: "u1", Kind: event.KindHuman},
		"invented auth":      {ID: "u1", Kind: event.KindAgent, Auth: event.AuthMethod("cookie")},
		"human with a PAT":   {ID: "u1", Kind: event.KindHuman, Auth: event.AuthPAT},
		"agent with session": {ID: "u1", Kind: event.KindAgent, Auth: event.AuthSession},
	}
	for name, a := range bad {
		t.Run(name, func(t *testing.T) {
			for op, write := range writesFor(a) {
				if err := write(); !errors.Is(err, errs.ErrValidation) {
					t.Errorf("%s with actor %+v = %v, want a validation error", op, a, err)
				}
			}
		})
	}

	// Positive control: a derived actor gets past the check for every write
	// and reaches the (absent) pool, so the refusals above are the actor
	// check's and not some earlier rule's.
	for _, a := range []event.Actor{agent("u1"), human("u1")} {
		for op, write := range writesFor(a) {
			if !reachesPool(write) {
				t.Errorf("%s with derived %s actor did not get past the actor check", op, a.Kind)
			}
		}
	}
}

// reachesPool reports whether write got past validation to the pool. The
// service under test has none, so reaching it panics.
func reachesPool(write func() error) (reached bool) {
	defer func() {
		if recover() != nil {
			reached = true
		}
	}()
	_ = write()
	return false
}
