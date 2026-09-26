package annotation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// Governing: ADR-0025, SPEC-0019 VE-1, VE-3, VE-6

// oneViolation asserts err carries exactly one violation on field with the
// given reason, that it still satisfies errors.Is against sentinel, and that
// it maps to validation_failed.
func oneViolation(t *testing.T, err error, sentinel error, field string, reason errs.Reason) errs.Violation {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v in the chain", err, sentinel)
	}
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("code = %s, want validation_failed", errs.CodeOf(err))
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 1 || vs[0].Field != field || vs[0].Reason != reason || vs[0].Location != errs.LocBody {
		t.Fatalf("violations = %+v, want one %s on %s", vs, reason, field)
	}
	return vs[0]
}

func TestEmojiViolations(t *testing.T) {
	oneViolation(t, validateEmoji(""), ErrEmojiInvalid, "emoji", errs.ReasonRequired)

	v := oneViolation(t, validateEmoji("🔥 🔥"), ErrEmojiInvalid, "emoji", errs.ReasonInvalidFormat)
	if v.Value == nil || *v.Value != "🔥 🔥" || !strings.Contains(v.Message, "a single emoji") {
		t.Fatalf("violation = %+v, want the emoji echoed and the rule stated", v)
	}
	v = oneViolation(t, validateEmoji(strings.Repeat("x", 200)), ErrEmojiInvalid, "emoji", errs.ReasonInvalidFormat)
	if v.Value == nil || len(*v.Value) != errs.MaxValueBytes {
		t.Fatalf("value = %v, want the echo capped at %d bytes", v.Value, errs.MaxValueBytes)
	}
}

func TestCommentBodyViolations(t *testing.T) {
	oneViolation(t, checkCommentBody(""), ErrBodyInvalid, "body", errs.ReasonRequired)

	v := oneViolation(t, checkCommentBody(strings.Repeat("a", maxCommentBytes+1)), ErrBodyInvalid, "body", errs.ReasonTooLong)
	if v.Limit != maxCommentBytes || v.Unit != errs.UnitBytes || v.Value != nil {
		t.Fatalf("violation = %+v, want a 16 KiB byte limit and no echo of the body", v)
	}
	if err := checkCommentBody(strings.Repeat("a", maxCommentBytes)); err != nil {
		t.Fatalf("a body at the limit was rejected: %v", err)
	}
}

func TestValidateAnchorViolations(t *testing.T) {
	reg := sharetype.Default()

	_, err := Validate(reg, 1, sharetype.KeyCode, sharetype.KindReaction, sharetype.AnchorImageRegion, json.RawMessage(`{"x":0.5,"y":0.5}`))
	v := oneViolation(t, err, ErrAnchorNotAllowed, "anchor_type", errs.ReasonNotAllowed)
	if v.Value == nil || *v.Value != "image_region" || !strings.Contains(v.Message, "code_line") {
		t.Fatalf("violation = %+v, want the anchor echoed and the code type's anchors named", v)
	}

	_, err = Validate(reg, 1, sharetype.KeyCode, sharetype.KindComment, "", nil)
	oneViolation(t, err, ErrAnchorNotAllowed, "anchor_type", errs.ReasonRequired)

	_, err = Validate(reg, 1, sharetype.KeyWebhook, sharetype.KindComment, sharetype.AnchorArtifact, nil)
	v = oneViolation(t, err, ErrNotCommentable, "anchor_type", errs.ReasonNotAllowed)
	if !strings.Contains(v.Message, "accepts no comments") || v.Value == nil || *v.Value != "artifact" {
		t.Fatalf("violation = %+v, want the anchor echoed and the type said to take no comments", v)
	}
	// An omitted anchor_type is not echoed as "".
	_, err = Validate(reg, 1, sharetype.KeyWebhook, sharetype.KindComment, "", nil)
	v = oneViolation(t, err, ErrNotCommentable, "anchor_type", errs.ReasonNotAllowed)
	if v.Value != nil || strings.Contains(v.Message, `""`) {
		t.Fatalf("violation = %+v, want no echo of an omitted anchor_type", v)
	}

	_, err = Validate(reg, 1, sharetype.KeyCode, sharetype.KindComment, sharetype.AnchorCodeLine, json.RawMessage(`{"line":0}`))
	v = oneViolation(t, err, ErrLocatorInvalid, "anchor_ref", errs.ReasonInvalidFormat)
	if v.Value != nil || !strings.Contains(v.Message, `"code_line"`) {
		t.Fatalf("violation = %+v, want the anchor_type named and the ref not echoed", v)
	}
}

func TestCheckReplyViolations(t *testing.T) {
	root := ReplyParent{Exists: true, AnchorType: sharetype.AnchorCodeLine, AnchorRef: json.RawMessage(`{"line":3}`), AnchorKey: `{"line":3}`}

	_, _, err := CheckReply(42, ReplyParent{}, "", nil)
	v := oneViolation(t, err, ErrParentNotFound, "parent_id", errs.ReasonNotAllowed)
	if v.Value == nil || *v.Value != "42" {
		t.Fatalf("violation = %+v, want the parent id echoed", v)
	}

	_, _, err = CheckReply(42, ReplyParent{Exists: true, IsReply: true}, "", nil)
	oneViolation(t, err, ErrThreadTooDeep, "parent_id", errs.ReasonNotAllowed)

	_, _, err = CheckReply(42, root, sharetype.AnchorArtifact, nil)
	oneViolation(t, err, ErrAnchorMismatch, "anchor_type", errs.ReasonMismatch)

	_, _, err = CheckReply(42, root, sharetype.AnchorCodeLine, json.RawMessage(`{"line":4}`))
	oneViolation(t, err, ErrAnchorMismatch, "anchor_ref", errs.ReasonMismatch)

	_, _, err = CheckReply(42, root, sharetype.AnchorCodeLine, json.RawMessage(`not json`))
	oneViolation(t, err, ErrLocatorInvalid, "anchor_ref", errs.ReasonInvalidFormat)

	// Inheriting and restating the root's anchor are both accepted.
	if at, ref, err := CheckReply(42, root, "", nil); err != nil || at != root.AnchorType || string(ref) != string(root.AnchorRef) {
		t.Fatalf("inherit = %s %s %v, want the root's anchor", at, ref, err)
	}
	if _, _, err := CheckReply(42, root, sharetype.AnchorCodeLine, json.RawMessage(`{ "line": 3 }`)); err != nil {
		t.Fatalf("restated anchor rejected: %v", err)
	}
}
