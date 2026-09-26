package annotation

import (
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/event"
)

// Governing: ADR-0022 (approval class), SPEC-0016 EV-5 "Approval Class and the
// Approval Bit", EV-6 "Legacy row is never an approval".

// TestDefaultApprovalClass pins the EV-5 default and its normalization: 👍 ✅
// ✔️ are members with or without a skin tone or VS-16, and nothing else is.
func TestDefaultApprovalClass(t *testing.T) {
	c := DefaultApprovalClass()
	for _, e := range []string{
		"👍", "👍🏻", "👍🏽", "👍🏿", // skin tones strip to the base
		"✅", "✅️",
		"✔️", "✔", // VS-16 strips either way
	} {
		if !c.Contains(e) {
			t.Errorf("default class does not contain %q (%U)", e, []rune(e))
		}
	}
	for _, e := range []string{"👎", "🎉", "❤️", "✓", "👍👍", ""} {
		if c.Contains(e) {
			t.Errorf("default class contains %q (%U), want not", e, []rune(e))
		}
	}
}

// TestApprovalBit is the EV-5 truth table: approval needs the class AND a
// stored human kind. An agent's and a legacy row's approval-class reaction is
// in the class but never an approval.
func TestApprovalBit(t *testing.T) {
	c := DefaultApprovalClass()
	cases := []struct {
		emoji        string
		stored       event.ActorKind
		wantClass    bool
		wantApproval bool
	}{
		{"👍🏽", event.KindHuman, true, true},
		{"✅", event.KindHuman, true, true},
		{"👍", event.KindAgent, true, false},
		{"👍", "", true, false}, // legacy row (EV-6)
		{"🎉", event.KindHuman, false, false},
		{"🎉", event.KindAgent, false, false},
		{"👍", "HUMAN", true, false}, // only the exact derived kind counts
	}
	for _, tc := range cases {
		class, approval := c.Approval(tc.emoji, tc.stored)
		if class != tc.wantClass || approval != tc.wantApproval {
			t.Errorf("Approval(%q, %q) = (%v, %v), want (%v, %v)",
				tc.emoji, tc.stored, class, approval, tc.wantClass, tc.wantApproval)
		}
	}
}

// TestNewApprovalClass covers CAIRN_APPROVAL_REACTIONS: a configured list
// replaces the default, entries are normalized, an empty list is the default,
// and an entry that is not an emoji fails rather than approving nothing.
func TestNewApprovalClass(t *testing.T) {
	c, err := NewApprovalClass([]string{" 🚀 ", "👌🏾", "✔️"})
	if err != nil {
		t.Fatalf("NewApprovalClass: %v", err)
	}
	for _, e := range []string{"🚀", "👌", "👌🏻", "✔"} {
		if !c.Contains(e) {
			t.Errorf("configured class does not contain %q", e)
		}
	}
	if c.Contains("👍") {
		t.Error("a configured class still contains the default 👍, want it replaced")
	}

	empty, err := NewApprovalClass(nil)
	if err != nil || !empty.Contains("👍") {
		t.Fatalf("NewApprovalClass(nil) = contains 👍 %v, err %v; want the default class", empty.Contains("👍"), err)
	}

	for _, bad := range [][]string{{"👍", "a b"}, {"️"}, {"🏽"}, {"x\x00"}} {
		if _, err := NewApprovalClass(bad); !errors.Is(err, ErrEmojiInvalid) {
			t.Errorf("NewApprovalClass(%q) err = %v, want ErrEmojiInvalid", bad, err)
		}
	}
}

// TestZeroOptionsUseDefaultClass: a Service built without an explicit class
// classifies with the documented default, never the empty class (which would
// silently turn every approval off).
func TestZeroOptionsUseDefaultClass(t *testing.T) {
	var zero ApprovalClass
	if zero.Contains("👍") {
		t.Fatal("positive control: the zero ApprovalClass must be empty")
	}
	svc := New(nil, Options{})
	if _, approval := svc.approval.Approval("👍", event.KindHuman); !approval {
		t.Fatal("New(nil, Options{}) does not classify a human 👍 as an approval")
	}
}
