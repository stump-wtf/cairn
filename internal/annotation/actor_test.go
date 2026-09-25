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
// the service boundary: every annotation write refuses an actor with no id or
// no derived kind, before it opens a transaction. The service here has no pool,
// so a write that got past the check would panic rather than pass.
//
// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-4.
func TestWritesRequireDerivedActor(t *testing.T) {
	svc := NewService(nil, nil)
	ctx := context.Background()

	bad := map[string]event.Actor{
		"no id":         {Kind: event.KindAgent, Auth: event.AuthPAT},
		"no kind":       {ID: "u1", Auth: event.AuthPAT},
		"invented kind": {ID: "u1", Kind: event.ActorKind("admin"), Auth: event.AuthSession},
	}
	for name, a := range bad {
		t.Run(name, func(t *testing.T) {
			writes := map[string]func() error{
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
			}
			for op, write := range writes {
				if err := write(); !errors.Is(err, errs.ErrValidation) {
					t.Errorf("%s with actor %+v = %v, want a validation error", op, a, err)
				}
			}
		})
	}

	// Positive control: a derived actor passes the check and the write fails on
	// the next rule instead, so the refusals above are the actor check's.
	for _, a := range []event.Actor{agent("u1"), human("u1")} {
		_, err := svc.AddComment(ctx, "SVCAAAA1", CommentInput{
			AnchorType: sharetype.AnchorArtifact, Actor: a, Body: "",
		})
		if !errors.Is(err, ErrBodyInvalid) {
			t.Errorf("AddComment(%s actor, empty body) = %v, want ErrBodyInvalid", a.Kind, err)
		}
	}
}
