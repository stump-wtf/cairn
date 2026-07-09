package sharetype

// Anchor is a location an annotation can attach to within an artifact of some
// share type. Whole-artifact is always legal for every type; the others are
// declared per type (SPEC-0002 REQ "Per-Type Anchor Affordances").
type Anchor string

const (
	// AnchorWholeArtifact is legal for every type and permits both reactions and
	// comments; the registry treats it as universally allowed.
	AnchorWholeArtifact Anchor = "whole_artifact"

	// Markdown anchors.
	AnchorMarkdownBlock  Anchor = "markdown_block"
	AnchorMarkdownBullet Anchor = "markdown_bullet"

	// A text selection range (markdown, code, trajectory).
	AnchorSelection Anchor = "selection"

	// Code anchors.
	AnchorCodeLine Anchor = "code_line"

	// Image anchors — a pinned region.
	AnchorImageRegion Anchor = "image_region"

	// Webhook anchors — a captured request. Reaction-only: reactable but never
	// comment-threaded (SPEC-0002 expresses this as a property of the type).
	AnchorWebhookRequest Anchor = "webhook_request"

	// Trajectory anchors — points in a captured agent run.
	AnchorTrajectorySpan     Anchor = "trajectory_span"
	AnchorTrajectoryTurn     Anchor = "trajectory_turn"
	AnchorTrajectoryToolCall Anchor = "trajectory_tool_call"
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
// webhook request); most content anchors permit both.
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
