// The approval class is the set of emoji whose reaction means "approved". A
// reaction's approval_class is membership in the set; its approval bit is that
// membership AND a stored actor kind of human, so only a click from a browser
// session is ever an approval. An agent's approval-class reaction is still
// accepted and stored; it just never carries the bit.
//
// Governing: ADR-0022 (approval class), SPEC-0016 EV-5 "Approval Class and the
// Approval Bit", EV-6 "Legacy row is never an approval".

package annotation

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/cairn/internal/event"
)

// DefaultApprovalReactions is the EV-5 default class: 👍 (U+1F44D), ✅
// (U+2705) and ✔️ (U+2714 U+FE0F).
var DefaultApprovalReactions = []string{"\U0001F44D", "✅", "✔️"}

// ApprovalClass is a normalized set of base emoji. The zero value is the
// empty class, in which nothing is an approval; use NewApprovalClass or
// DefaultApprovalClass to build a populated one.
type ApprovalClass struct {
	set map[string]bool
}

// NewApprovalClass normalizes and validates the configured emoji
// (CAIRN_APPROVAL_REACTIONS, already split on commas). An empty list is the
// EV-5 default. An entry that is not a plausible emoji once normalized is an
// error, so a typo fails startup instead of silently approving nothing.
func NewApprovalClass(emoji []string) (ApprovalClass, error) {
	if len(emoji) == 0 {
		emoji = DefaultApprovalReactions
	}
	c := ApprovalClass{set: make(map[string]bool, len(emoji))}
	for _, e := range emoji {
		base := NormalizeEmoji(strings.TrimSpace(e))
		if err := validateEmoji(base); err != nil {
			return ApprovalClass{}, fmt.Errorf("approval reaction %q: %w", e, err)
		}
		c.set[base] = true
	}
	return c, nil
}

// DefaultApprovalClass is the class built from DefaultApprovalReactions.
func DefaultApprovalClass() ApprovalClass {
	c, err := NewApprovalClass(nil)
	if err != nil {
		// The defaults are constants validated by TestDefaultApprovalClass.
		panic(err)
	}
	return c
}

// Contains reports whether emoji, normalized, is in the class.
func (c ApprovalClass) Contains(emoji string) bool {
	return c.set[NormalizeEmoji(emoji)]
}

// Approval returns the EV-5 pair for a reaction row: approval_class is
// membership, and approval additionally requires the row's STORED kind to be
// human. A legacy row (kind "") and an agent's row are never approvals.
func (c ApprovalClass) Approval(emoji string, stored event.ActorKind) (class, approval bool) {
	class = c.Contains(emoji)
	return class, class && stored == event.KindHuman
}

// NormalizeEmoji strips the variation selector-16 (U+FE0F) and the Fitzpatrick
// skin-tone modifiers (U+1F3FB..U+1F3FF), so 👍🏽 and ✔️ match their base
// emoji (SPEC-0016 EV-5).
func NormalizeEmoji(emoji string) string {
	return strings.Map(func(r rune) rune {
		if r == '️' || (r >= 0x1F3FB && r <= 0x1F3FF) {
			return -1
		}
		return r
	}, emoji)
}
