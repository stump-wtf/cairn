package clicmd

import (
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliexit"
)

func TestParseTTLFlagEmptyMeansUnset(t *testing.T) {
	secs, err := parseTTLFlag("")
	if err != nil {
		t.Fatalf("parseTTLFlag(\"\"): %v", err)
	}
	if secs != 0 {
		t.Errorf("secs = %d, want 0", secs)
	}
}

func TestParseTTLFlagBareIntegerIsSeconds(t *testing.T) {
	secs, err := parseTTLFlag("3600")
	if err != nil {
		t.Fatalf("parseTTLFlag: %v", err)
	}
	if secs != 3600 {
		t.Errorf("secs = %d, want 3600", secs)
	}
}

func TestParseTTLFlagGoDuration(t *testing.T) {
	secs, err := parseTTLFlag("24h")
	if err != nil {
		t.Fatalf("parseTTLFlag: %v", err)
	}
	if secs != 24*3600 {
		t.Errorf("secs = %d, want %d", secs, 24*3600)
	}
}

func TestParseTTLFlagDaySuffix(t *testing.T) {
	secs, err := parseTTLFlag("7d")
	if err != nil {
		t.Fatalf("parseTTLFlag: %v", err)
	}
	if want := int64(7 * 24 * 3600); secs != want {
		t.Errorf("secs = %d, want %d", secs, want)
	}
}

func TestParseTTLFlagRejectsGarbage(t *testing.T) {
	_, err := parseTTLFlag("not-a-duration")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, cliexit.ErrUsage) {
		t.Errorf("err = %v, want it to wrap cliexit.ErrUsage", err)
	}
}

func TestParseTTLFlagRejectsNonPositive(t *testing.T) {
	for _, s := range []string{"0", "-5", "-1h", "0d"} {
		if _, err := parseTTLFlag(s); err == nil {
			t.Errorf("parseTTLFlag(%q): want an error", s)
		}
	}
}

func TestFormatAccess(t *testing.T) {
	cases := map[string]string{
		"link":    "you + anyone with link",
		"private": "only you",
		"":        "unknown",
	}
	for in, want := range cases {
		if got := formatAccess(in); got != want {
			t.Errorf("formatAccess(%q) = %q, want %q", in, got, want)
		}
	}
}
