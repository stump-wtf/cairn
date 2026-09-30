package trajectory

import (
	"context"
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
)

// TestCloseRunRequiresDerivedActor pins the fail-closed rule for the closer:
// CloseRun refuses an actor with no id, no derived kind, no auth method, or a
// kind that contradicts its auth method, before it opens a transaction. The
// service has no pool, so a close that got past the check would panic rather
// than pass.
//
// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-3, EV-4.
func TestCloseRunRequiresDerivedActor(t *testing.T) {
	svc := NewService(nil, nil, Options{})
	for name, a := range map[string]event.Actor{
		"no id":              {Kind: event.KindAgent, Auth: event.AuthPAT},
		"no kind":            {ID: "joe", Auth: event.AuthPAT},
		"invented kind":      {ID: "joe", Kind: event.ActorKind("admin"), Auth: event.AuthSession},
		"no auth":            {ID: "joe", Kind: event.KindAgent},
		"invented auth":      {ID: "joe", Kind: event.KindAgent, Auth: event.AuthMethod("cookie")},
		"human with OAuth":   {ID: "joe", Kind: event.KindHuman, Auth: event.AuthOAuth},
		"agent with session": {ID: "joe", Kind: event.KindAgent, Auth: event.AuthSession},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.CloseRun(context.Background(), "RUNAAAA1", a); !errors.Is(err, errs.ErrValidation) {
				t.Errorf("CloseRun(%+v) = %v, want a validation error", a, err)
			}
		})
	}

	// Positive control: a derived actor gets past the check and reaches the
	// (absent) pool, so the refusals above are the actor check's.
	for _, a := range []event.Actor{
		{ID: "joe", Kind: event.KindAgent, Auth: event.AuthOAuth},
		{ID: "joe", Kind: event.KindHuman, Auth: event.AuthSession},
	} {
		if !closeReachesPool(svc, a) {
			t.Errorf("CloseRun with derived %s actor did not get past the actor check", a.Kind)
		}
	}
}

// closeReachesPool reports whether CloseRun got past validation to the pool.
// The service under test has none, so reaching it panics.
func closeReachesPool(svc *Service, a event.Actor) (reached bool) {
	defer func() {
		if recover() != nil {
			reached = true
		}
	}()
	_, _ = svc.CloseRun(context.Background(), "RUNAAAA1", a)
	return false
}
