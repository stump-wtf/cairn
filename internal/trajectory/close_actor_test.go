package trajectory

import (
	"context"
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
)

// TestCloseRunRequiresDerivedActor pins the fail-closed rule for the closer:
// CloseRun refuses an actor with no id or no derived kind before it opens a
// transaction. The service has no pool, so a close that got past the check
// would panic rather than pass.
//
// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-4.
func TestCloseRunRequiresDerivedActor(t *testing.T) {
	svc := NewService(nil, nil, Options{})
	for name, a := range map[string]event.Actor{
		"no id":         {Kind: event.KindAgent, Auth: event.AuthPAT},
		"no kind":       {ID: "joe", Auth: event.AuthPAT},
		"invented kind": {ID: "joe", Kind: event.ActorKind("admin"), Auth: event.AuthSession},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.CloseRun(context.Background(), "RUNAAAA1", a); !errors.Is(err, errs.ErrValidation) {
				t.Errorf("CloseRun(%+v) = %v, want a validation error", a, err)
			}
		})
	}
}
