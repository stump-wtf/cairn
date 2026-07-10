package sharetype

// Anchor is a location an annotation can attach to within an artifact of some
// share type. Anchor values are the registry-owned `anchor_type` discriminator
// strings SPEC-0006 stores alongside each annotation, so the constants here MUST
// match the capability matrix in the annotations design doc exactly. Every
// anchor a type permits — including the whole-artifact `artifact` anchor — is
// declared per type as registry data (SPEC-0002 REQ "Per-Type Anchor
// Affordances"); nothing is universally implied, which is how the webhook type
// rejects comments even at the whole-artifact level (SPEC-0006 REQ "Webhook
// Reaction-Only Asymmetry").
//
// Governing: ADR-0002 (anchor affordances are per-type registry data), ADR-0006
// (polymorphic anchor model), SPEC-0006 REQ "Registry-Gated Anchor Capabilities".
type Anchor string

const (
	// AnchorArtifact targets the whole artifact. Its anchor_ref is the empty
	// object `{}` (SPEC-0006 "Whole-artifact anchor"). Most types permit both
	// kinds on it; whether they actually do is per-type registry data.
	AnchorArtifact Anchor = "artifact"

	// Markdown anchors.
	AnchorMarkdownBlock  Anchor = "md_block"
	AnchorMarkdownBullet Anchor = "md_bullet"

	// AnchorTextSelection is a character-offset range storing its quoted
	// substring (markdown, code, trajectory transcripts).
	AnchorTextSelection Anchor = "text_selection"

	// Code anchors — a single line, or a line range.
	AnchorCodeLine  Anchor = "code_line"
	AnchorCodeRange Anchor = "code_range"

	// Image anchors — a pinned region in normalized fractional coordinates.
	AnchorImageRegion Anchor = "image_region"

	// Webhook anchors — a captured request. Reaction-only: reactable but never
	// comment-threaded (SPEC-0006 expresses this as a property of the type).
	AnchorWebhookRequest Anchor = "webhook_request"

	// Trajectory anchors — points in a captured agent run.
	AnchorTrajectorySpan     Anchor = "trajectory_span"
	AnchorTrajectoryTurn     Anchor = "trajectory_turn"
	AnchorTrajectoryToolCall Anchor = "trajectory_toolcall"
)

// AnnotationKind distinguishes the two annotation kinds an anchor may permit
// (SPEC-0006 owns their storage; this package owns which anchors permit which).
type AnnotationKind string

const (
	KindReaction AnnotationKind = "reaction"
	KindComment  AnnotationKind = "comment"
)

// AnchorSpec declares one legal anchor for a type and which annotation kinds it
// permits. A reaction-only anchor sets Reactions=true, Comments=false (e.g. a
// webhook request or a trajectory turn); a comment-only anchor is the inverse
// (e.g. a text selection); whole-artifact anchors usually permit both.
type AnchorSpec struct {
	Anchor    Anchor
	Reactions bool
	Comments  bool
}

// both is a convenience for an anchor permitting reactions and comments.
func both(a Anchor) AnchorSpec { return AnchorSpec{Anchor: a, Reactions: true, Comments: true} }

// reactionOnly is a convenience for a reactable-but-not-commentable anchor.
func reactionOnly(a Anchor) AnchorSpec {
	return AnchorSpec{Anchor: a, Reactions: true, Comments: false}
}

// commentOnly is a convenience for a commentable-but-not-reactable anchor.
func commentOnly(a Anchor) AnchorSpec {
	return AnchorSpec{Anchor: a, Reactions: false, Comments: true}
}
