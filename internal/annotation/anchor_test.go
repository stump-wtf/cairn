package annotation

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

func TestCanonicalKeyOrderInvariance(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"flat keys", `{"start":40,"end":47}`, `{"end":47,"start":40}`},
		{"whitespace", `{ "line" : 42 }`, `{"line":42}`},
		{"nested", `{"a":{"y":2,"x":1},"b":[1,2]}`, `{"b":[1,2],"a":{"x":1,"y":2}}`},
		{
			"selection",
			`{"quote":"then we deploy","start":1201,"end":1240}`,
			`{"end":1240,"start":1201,"quote":"then we deploy"}`,
		},
	}
	for _, tc := range cases {
		ka, err := CanonicalKey(json.RawMessage(tc.a))
		if err != nil {
			t.Fatalf("%s: canonical(%s): %v", tc.name, tc.a, err)
		}
		kb, err := CanonicalKey(json.RawMessage(tc.b))
		if err != nil {
			t.Fatalf("%s: canonical(%s): %v", tc.name, tc.b, err)
		}
		if ka != kb {
			t.Errorf("%s: refs differing only in key order/whitespace produced different keys:\n  %s\n  %s", tc.name, ka, kb)
		}
	}
}

func TestCanonicalKeyForm(t *testing.T) {
	cases := []struct {
		ref  string
		want string
	}{
		{``, `{}`},
		{`null`, `{}`},
		{`{}`, `{}`},
		{`{"b":2,"a":1}`, `{"a":1,"b":2}`},
		{`{"x":0.42,"y":0.31}`, `{"x":0.42,"y":0.31}`},
		{`{"path":[2,0],"block_id":"b_9c1"}`, `{"block_id":"b_9c1","path":[2,0]}`},
	}
	for _, tc := range cases {
		got, err := CanonicalKey(json.RawMessage(tc.ref))
		if err != nil {
			t.Errorf("canonical(%q): %v", tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("canonical(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestCanonicalKeyRejectsNonObjects(t *testing.T) {
	for _, ref := range []string{`[]`, `[1,2]`, `"str"`, `42`, `true`, `{"a":1} {"b":2}`, `{"a":`} {
		if _, err := CanonicalKey(json.RawMessage(ref)); err == nil {
			t.Errorf("canonical(%q) accepted, want error", ref)
		}
	}
}

func TestCanonicalKeyDistinguishesDifferentLocators(t *testing.T) {
	ka, err := CanonicalKey(json.RawMessage(`{"line":42}`))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := CanonicalKey(json.RawMessage(`{"line":43}`))
	if err != nil {
		t.Fatal(err)
	}
	if ka == kb {
		t.Error("different locators must not share a canonical key")
	}
}

// validRefs supplies a schema-valid anchor_ref per anchor type for matrix
// tests.
var validRefs = map[sharetype.Anchor]string{
	sharetype.AnchorArtifact:           `{}`,
	sharetype.AnchorMarkdownBlock:      `{"block_id":"b_3f2a9c81d04e"}`,
	sharetype.AnchorMarkdownBullet:     `{"block_id":"b_9c1","path":[2,0]}`,
	sharetype.AnchorTextSelection:      `{"start":1201,"end":1240,"quote":"then we deploy"}`,
	sharetype.AnchorCodeLine:           `{"line":42}`,
	sharetype.AnchorCodeRange:          `{"start":40,"end":47}`,
	sharetype.AnchorImageRegion:        `{"x":0.42,"y":0.31}`,
	sharetype.AnchorWebhookRequest:     `{"request_id":"req_7Kx9"}`,
	sharetype.AnchorTrajectorySpan:     `{"span_id":"sp_c3aa"}`,
	sharetype.AnchorTrajectoryTurn:     `{"span_id":"sp_a1bb"}`,
	sharetype.AnchorTrajectoryToolCall: `{"span_id":"sp_b2cc"}`,
}

// allAnchors is the full built-in anchor vocabulary, so the matrix test also
// exercises every combination a type does NOT declare.
var allAnchors = []sharetype.Anchor{
	sharetype.AnchorArtifact,
	sharetype.AnchorMarkdownBlock,
	sharetype.AnchorMarkdownBullet,
	sharetype.AnchorTextSelection,
	sharetype.AnchorCodeLine,
	sharetype.AnchorCodeRange,
	sharetype.AnchorImageRegion,
	sharetype.AnchorWebhookRequest,
	sharetype.AnchorTrajectorySpan,
	sharetype.AnchorTrajectoryTurn,
	sharetype.AnchorTrajectoryToolCall,
}

// anchorMatrix restates the SPEC-0006 capability matrix independently of
// builtin.go, so a registry drift fails here (ADR-0006 "a per-share-type
// anchor capability matrix test asserts that each registry entry accepts
// exactly its declared reaction/comment anchor types and rejects all others").
var anchorMatrix = map[artifact.ShareType]struct {
	reactions []sharetype.Anchor
	comments  []sharetype.Anchor
}{
	artifact.TypeFile: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact},
	},
	artifact.TypeGZ: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact},
	},
	sharetype.KeyMarkdown: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorMarkdownBlock, sharetype.AnchorMarkdownBullet},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorTextSelection},
	},
	sharetype.KeyCode: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorCodeLine, sharetype.AnchorCodeRange},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorCodeLine, sharetype.AnchorTextSelection},
	},
	sharetype.KeyImage: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorImageRegion},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorImageRegion},
	},
	artifact.TypeBundle: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorTextSelection},
	},
	sharetype.KeyWebhook: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorWebhookRequest},
		comments:  nil, // the deliberate asymmetry: no comment anchors at all
	},
	sharetype.KeyTrajectory: {
		reactions: []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorTrajectoryTurn, sharetype.AnchorTrajectoryToolCall},
		comments:  []sharetype.Anchor{sharetype.AnchorArtifact, sharetype.AnchorTrajectorySpan, sharetype.AnchorTextSelection},
	},
}

func contains(set []sharetype.Anchor, a sharetype.Anchor) bool {
	for _, x := range set {
		if x == a {
			return true
		}
	}
	return false
}

// TestValidateAnchorMatrix drives Validate over every built-in share type ×
// anchor × kind combination: legal combos canonicalize, illegal combos are
// rejected with the right sentinel and never reach persistence.
func TestValidateAnchorMatrix(t *testing.T) {
	reg := sharetype.Default()
	for shareType, caps := range anchorMatrix {
		for _, anchor := range allAnchors {
			for kind, allowedSet := range map[sharetype.AnnotationKind][]sharetype.Anchor{
				sharetype.KindReaction: caps.reactions,
				sharetype.KindComment:  caps.comments,
			} {
				ref := json.RawMessage(validRefs[anchor])
				got, err := Validate(reg, 1, shareType, kind, anchor, ref)
				if contains(allowedSet, anchor) {
					if err != nil {
						t.Errorf("%s/%s/%s: legal combo rejected: %v", shareType, kind, anchor, err)
						continue
					}
					if got.ArtifactID != 1 || got.Type != anchor {
						t.Errorf("%s/%s/%s: anchor fields = %+v", shareType, kind, anchor, got)
					}
					wantKey, _ := CanonicalKey(ref)
					if got.Key != wantKey || string(got.Ref) != wantKey {
						t.Errorf("%s/%s/%s: key %q / ref %q, want canonical %q", shareType, kind, anchor, got.Key, got.Ref, wantKey)
					}
					continue
				}
				if err == nil {
					t.Errorf("%s/%s/%s: illegal combo accepted", shareType, kind, anchor)
					continue
				}
				wantSentinel := error(ErrAnchorNotAllowed)
				if kind == sharetype.KindComment && len(caps.comments) == 0 {
					wantSentinel = ErrNotCommentable
				}
				if !errors.Is(err, wantSentinel) {
					t.Errorf("%s/%s/%s: error %v, want sentinel %v", shareType, kind, anchor, err, wantSentinel)
				}
				if errs.CodeOf(err) != errs.CodeValidation {
					t.Errorf("%s/%s/%s: code %q, want validation_failed", shareType, kind, anchor, errs.CodeOf(err))
				}
			}
		}
	}
}

// TestValidateWebhookAsymmetry pins the SPEC-0006 invariant explicitly: a
// comment on ANY webhook anchor is refused while a webhook_request reaction is
// accepted.
func TestValidateWebhookAsymmetry(t *testing.T) {
	reg := sharetype.Default()
	if _, err := Validate(reg, 1, sharetype.KeyWebhook, sharetype.KindReaction,
		sharetype.AnchorWebhookRequest, json.RawMessage(`{"request_id":"req_7Kx9"}`)); err != nil {
		t.Fatalf("webhook_request reaction must be accepted: %v", err)
	}
	for _, anchor := range allAnchors {
		_, err := Validate(reg, 1, sharetype.KeyWebhook, sharetype.KindComment,
			anchor, json.RawMessage(validRefs[anchor]))
		if !errors.Is(err, ErrNotCommentable) {
			t.Errorf("comment on webhook %q anchor: err = %v, want ErrNotCommentable", anchor, err)
		}
	}
}

// TestValidateLocatorFailure asserts a capability-legal anchor with a
// schema-invalid ref maps to ErrLocatorInvalid / validation_failed.
func TestValidateLocatorFailure(t *testing.T) {
	reg := sharetype.Default()
	cases := []struct {
		shareType artifact.ShareType
		kind      sharetype.AnnotationKind
		anchor    sharetype.Anchor
		ref       string
	}{
		{sharetype.KeyMarkdown, sharetype.KindReaction, sharetype.AnchorMarkdownBlock, `{"block_id":""}`},
		{sharetype.KeyCode, sharetype.KindComment, sharetype.AnchorCodeLine, `{"line":0}`},
		{sharetype.KeyImage, sharetype.KindReaction, sharetype.AnchorImageRegion, `{"x":420,"y":310}`},
		{sharetype.KeyWebhook, sharetype.KindReaction, sharetype.AnchorWebhookRequest, `{}`},
		{sharetype.KeyTrajectory, sharetype.KindComment, sharetype.AnchorTrajectorySpan, `{"span_id":7}`},
		{artifact.TypeFile, sharetype.KindReaction, sharetype.AnchorArtifact, `{"unexpected":1}`},
		{sharetype.KeyMarkdown, sharetype.KindComment, sharetype.AnchorTextSelection, `not json`},
	}
	for _, tc := range cases {
		_, err := Validate(reg, 1, tc.shareType, tc.kind, tc.anchor, json.RawMessage(tc.ref))
		if !errors.Is(err, ErrLocatorInvalid) {
			t.Errorf("%s/%s/%s ref %q: err = %v, want ErrLocatorInvalid", tc.shareType, tc.kind, tc.anchor, tc.ref, err)
		}
		if errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("%s/%s ref %q: code %q, want validation_failed", tc.shareType, tc.anchor, tc.ref, errs.CodeOf(err))
		}
	}
}

// TestValidateUnknownTypeFallsBack asserts an unregistered share type resolves
// to the generic file floor: whole-artifact annotations only.
func TestValidateUnknownTypeFallsBack(t *testing.T) {
	reg := sharetype.Default()
	if _, err := Validate(reg, 1, "mystery", sharetype.KindReaction,
		sharetype.AnchorArtifact, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unknown type whole-artifact reaction must be accepted: %v", err)
	}
	_, err := Validate(reg, 1, "mystery", sharetype.KindComment,
		sharetype.AnchorCodeLine, json.RawMessage(`{"line":42}`))
	if !errors.Is(err, ErrAnchorNotAllowed) {
		t.Fatalf("unknown type code_line comment: err = %v, want ErrAnchorNotAllowed", err)
	}
}

// TestValidateWholeArtifactRefIsEmptyObject pins the SPEC-0006 rule that the
// whole-artifact anchor_ref is {}, whether supplied as empty, null, or {}.
func TestValidateWholeArtifactRefIsEmptyObject(t *testing.T) {
	reg := sharetype.Default()
	for _, ref := range []string{``, `{}`, `null`, ` { } `} {
		a, err := Validate(reg, 1, artifact.TypeFile, sharetype.KindComment,
			sharetype.AnchorArtifact, json.RawMessage(ref))
		if err != nil {
			t.Errorf("artifact anchor with ref %q rejected: %v", ref, err)
			continue
		}
		if a.Key != "{}" || string(a.Ref) != "{}" {
			t.Errorf("artifact anchor ref %q canonicalized to %q, want {}", ref, a.Key)
		}
	}
}
