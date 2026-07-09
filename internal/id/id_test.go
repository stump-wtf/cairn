package id

import (
	"strings"
	"testing"
)

func TestNewDefaultLengthAndAlphabet(t *testing.T) {
	const iterations = 2000
	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		got, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if len(got) != DefaultLength {
			t.Fatalf("length = %d, want %d (%q)", len(got), DefaultLength, got)
		}
		for _, r := range got {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("id %q contains non-base62 rune %q", got, r)
			}
		}
		if _, dup := seen[got]; dup {
			t.Fatalf("duplicate id %q within %d draws", got, iterations)
		}
		seen[got] = struct{}{}
		if IsReserved(got) {
			t.Fatalf("generator returned reserved word %q", got)
		}
	}
}

func TestNewLengthValidation(t *testing.T) {
	if _, err := NewLength(0); err == nil {
		t.Fatal("NewLength(0) should error")
	}
	got, err := NewLength(12)
	if err != nil {
		t.Fatalf("NewLength(12): %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("length = %d, want 12", len(got))
	}
}

func TestIsReserved(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"run", true},
		{"hook", true},
		{"api", true},
		{".well-known", true},
		{"healthz", true},
		{"9qz1aB2c", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsReserved(tt.in); got != tt.want {
			t.Errorf("IsReserved(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestDistribution is a coarse sanity check that every alphabet character can be
// produced, guarding against an off-by-one in the sampling range.
func TestDistribution(t *testing.T) {
	counts := make(map[rune]int)
	for i := 0; i < 5000; i++ {
		s, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, r := range s {
			counts[r]++
		}
	}
	if len(counts) < 55 { // 62 possible; allow slack for randomness
		t.Fatalf("only %d distinct characters observed, sampling range looks wrong", len(counts))
	}
}
