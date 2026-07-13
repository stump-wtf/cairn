package cliexit

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/joestump/cairn/internal/cliclient"
)

func TestForErrorNil(t *testing.T) {
	if got := ForError(nil); got != Success {
		t.Errorf("ForError(nil) = %d, want %d", got, Success)
	}
}

func TestForErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"usage sentinel", ErrUsage, Usage},
		{"wrapped usage", fmt.Errorf("bad flag: %w", ErrUsage), Usage},
		{"interrupted sentinel", ErrInterrupted, Interrupted},
		{"context canceled", context.Canceled, Interrupted},
		{"wrapped context canceled", fmt.Errorf("request: %w", context.Canceled), Interrupted},
		{"network sentinel", cliclient.ErrNetwork, Network},
		{"wrapped network", fmt.Errorf("dial: %w", cliclient.ErrNetwork), Network},
		{"unauthorized api error", cliclient.ErrNotAuthenticated, NotAuthenticated},
		{"forbidden api error", cliclient.ErrForbidden, Forbidden},
		{"not found api error", cliclient.ErrNotFound, NotFound},
		{"expired aliases not found", cliclient.ErrExpired, NotFound},
		{"oversize api error", cliclient.ErrOversize, Oversize},
		{"rate limited api error", cliclient.ErrRateLimited, RateLimited},
		{"conflict api error", cliclient.ErrConflict, Conflict},
		{"validation api error maps to usage", cliclient.ErrValidation, Usage},
		{"server internal api error", cliclient.ErrServerInternal, Internal},
		{"unclassified error", errors.New("boom"), Internal},
		{
			"decoded api error with request id still classified by code",
			&cliclient.APIError{Code: cliclient.CodeNotFound, Message: "gone", RequestID: "req_1"},
			NotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ForError(tt.err); got != tt.want {
				t.Errorf("ForError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestExitCodeValuesAreStable(t *testing.T) {
	// These values are normative (scripts grep/compare on them) — pin them
	// explicitly so an accidental renumbering fails CI.
	want := map[Code]int{
		Success: 0, Internal: 1, Usage: 2, NotAuthenticated: 3, Forbidden: 4,
		NotFound: 5, Oversize: 6, RateLimited: 7, Network: 8, Conflict: 9,
		Interrupted: 130,
	}
	for code, val := range want {
		if int(code) != val {
			t.Errorf("Code %v = %d, want %d", code, int(code), val)
		}
	}
}
