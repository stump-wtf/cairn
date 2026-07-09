package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeOfWrappedSentinels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"nil", nil, ""},
		{"plain is internal", errors.New("boom"), CodeInternal},
		{"not found", ErrNotFound, CodeNotFound},
		{"wrapped not found", fmt.Errorf("load artifact abc: %w", ErrNotFound), CodeNotFound},
		{"deeply wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrTooLarge)), CodePayloadTooLarge},
		{"checksum mismatch is validation", ErrChecksumMismatch, CodeValidation},
		{"validationf", Validationf("bad %s", "field"), CodeValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeOf(tt.err); got != tt.want {
				t.Fatalf("CodeOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrorsIsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("resolve artifact %s: %w", "9qz1a", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("errors.Is should find ErrNotFound through the wrap")
	}
	if errors.Is(wrapped, ErrConflict) {
		t.Fatal("errors.Is should not match a different sentinel")
	}
}

func TestValidationfIsValidation(t *testing.T) {
	err := Validationf("missing %s", "title")
	if !errors.Is(err, ErrValidation) {
		t.Fatal("Validationf should wrap ErrValidation")
	}
}
