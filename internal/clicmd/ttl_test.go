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

// TestFormatTTLSecondsRoundTrips: a limit renders in the largest whole unit,
// in syntax --ttl reads back as the same number of seconds (SPEC-0019 VE-8).
func TestFormatTTLSecondsRoundTrips(t *testing.T) {
	cases := map[int64]string{
		2592000: "30d",
		86400:   "1d",
		90000:   "25h",
		5400:    "90m",
		3600:    "1h",
		61:      "61s",
		1:       "1s",
	}
	for secs, want := range cases {
		got := formatTTLSeconds(secs)
		if got != want {
			t.Errorf("formatTTLSeconds(%d) = %q, want %q", secs, got, want)
		}
		back, err := parseTTLFlag(got)
		if err != nil || back != secs {
			t.Errorf("parseTTLFlag(%q) = %d, %v; want %d", got, back, err, secs)
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
