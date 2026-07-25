package annotation

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateEmoji pins the service-level emoji invariant: non-empty,
// bounded, no whitespace/control — while multi-rune ZWJ sequences (a single
// grapheme, several runes) stay legal since arbitrary emoji are allowed.
func TestValidateEmoji(t *testing.T) {
	valid := []string{
		"🔥", "👀", "🎉",
		"👍🏽",      // skin-tone modifier sequence
		"👩‍👩‍👧‍👧", // ZWJ family sequence
		"❤️",      // variation selector
	}
	for _, e := range valid {
		if err := validateEmoji(e); err != nil {
			t.Errorf("validateEmoji(%q) = %v, want nil", e, err)
		}
	}

	invalid := []string{
		"",                                   // empty
		" ",                                  // whitespace
		"🔥 🔥",                                // embedded space
		"a\nb",                               // control character
		"\x80",                               // invalid UTF-8
		strings.Repeat("🔥", 17),              // too many runes
		strings.Repeat("x", maxEmojiBytes+1), // too many bytes
	}
	for _, e := range invalid {
		err := validateEmoji(e)
		if err == nil {
			t.Errorf("validateEmoji(%q) = nil, want ErrEmojiInvalid", e)
			continue
		}
		if !errors.Is(err, ErrEmojiInvalid) {
			t.Errorf("validateEmoji(%q) = %v, want ErrEmojiInvalid in chain", e, err)
		}
	}
}
