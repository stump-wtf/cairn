package sharetype

// Per-anchor locator schemas for the built-in anchor vocabulary.
//
// Each anchor_type's anchor_ref is a small JSONB locator whose shape is fixed
// by ADR-0006's anchor table; the annotation layer rejects on write any
// anchor_ref that fails its anchor's schema (SPEC-0006 REQ "Registry-Gated
// Anchor Capabilities": "reject an anchor_ref that fails that anchor_type's
// locator schema"). The built-in share types supply these schemas through the
// LocatorSchemer capability (capability.go), so validation stays registry
// data: a third-party type registers its own schemas the same way and the
// annotation service never switches on anchor names.
//
// Schemas are strict — unknown fields are rejected — so a malformed or
// misspelled locator can never persist and later fail to resolve (ADR-0006
// "a registry bug could persist a malformed anchor. Mitigated by validating
// against the registry schema on write").
//
// Governing: ADR-0002 (capabilities are registry data), ADR-0006 (the anchor
// model: anchor_ref shapes), SPEC-0006 REQ "Registry-Gated Anchor
// Capabilities", REQ "Anchor Stability".

import (
	"bytes"
	"encoding/json"

	"github.com/joestump/cairn/internal/errs"
)

// defaultLocators maps each built-in anchor to its locator schema, per the
// ADR-0006 anchor_ref shape table. AnchorArtifact is absent deliberately: the
// registry itself enforces its empty-object rule (ValidateLocator), because
// that rule is type-agnostic.
var defaultLocators = map[Anchor]LocatorFunc{
	AnchorMarkdownBlock:      mdBlockLocator,
	AnchorMarkdownBullet:     mdBulletLocator,
	AnchorTextSelection:      textSelectionLocator,
	AnchorCodeLine:           codeLineLocator,
	AnchorCodeRange:          codeRangeLocator,
	AnchorImageRegion:        imageRegionLocator,
	AnchorWebhookRequest:     webhookRequestLocator,
	AnchorTrajectorySpan:     spanIDLocator(AnchorTrajectorySpan),
	AnchorTrajectoryTurn:     spanIDLocator(AnchorTrajectoryTurn),
	AnchorTrajectoryToolCall: spanIDLocator(AnchorTrajectoryToolCall),
}

// LocatorSchema implements the LocatorSchemer capability for every built-in
// type (prefixedType and codeShareType inherit it by embedding). Anchors a
// type does not declare never reach the schema: capability-set validation
// rejects them first.
func (t simpleType) LocatorSchema(anchor Anchor) LocatorFunc {
	return defaultLocators[anchor]
}

// decodeStrict unmarshals ref into v rejecting unknown fields, non-object
// payloads, and trailing garbage, returning a validation-coded error.
func decodeStrict(anchor Anchor, ref json.RawMessage, v any) error {
	if len(bytes.TrimSpace(ref)) == 0 {
		return errs.Validationf("sharetype: %q anchor_ref must be a JSON object, got empty", anchor)
	}
	dec := json.NewDecoder(bytes.NewReader(ref))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errs.Validationf("sharetype: %q anchor_ref does not match its locator schema: %v", anchor, err)
	}
	if dec.More() {
		return errs.Validationf("sharetype: %q anchor_ref has trailing data", anchor)
	}
	return nil
}

// mdBlockLocator validates {"block_id":"b_3f2a"} — a deterministic markdown
// block id (see internal/annotation.BlockID).
func mdBlockLocator(ref json.RawMessage) error {
	var loc struct {
		BlockID *string `json:"block_id"`
	}
	if err := decodeStrict(AnchorMarkdownBlock, ref, &loc); err != nil {
		return err
	}
	if loc.BlockID == nil || *loc.BlockID == "" {
		return errs.Validationf("sharetype: %q anchor_ref requires a non-empty block_id", AnchorMarkdownBlock)
	}
	return nil
}

// mdBulletLocator validates {"block_id":"b_9c1","path":[2,0]} — a block id
// extended with an ordinal path into the bullet tree.
func mdBulletLocator(ref json.RawMessage) error {
	var loc struct {
		BlockID *string `json:"block_id"`
		Path    []int   `json:"path"`
	}
	if err := decodeStrict(AnchorMarkdownBullet, ref, &loc); err != nil {
		return err
	}
	if loc.BlockID == nil || *loc.BlockID == "" {
		return errs.Validationf("sharetype: %q anchor_ref requires a non-empty block_id", AnchorMarkdownBullet)
	}
	if len(loc.Path) == 0 {
		return errs.Validationf("sharetype: %q anchor_ref requires a non-empty ordinal path", AnchorMarkdownBullet)
	}
	for _, ord := range loc.Path {
		if ord < 0 {
			return errs.Validationf("sharetype: %q anchor_ref path ordinals must be >= 0", AnchorMarkdownBullet)
		}
	}
	return nil
}

// textSelectionLocator validates {"start":1201,"end":1240,"quote":"…"} —
// character offsets plus the quoted substring, stored together so a selection
// can be re-highlighted and flagged "context changed" when the quote no longer
// matches (SPEC-0006 REQ "Anchor Stability").
func textSelectionLocator(ref json.RawMessage) error {
	var loc struct {
		Start *int    `json:"start"`
		End   *int    `json:"end"`
		Quote *string `json:"quote"`
	}
	if err := decodeStrict(AnchorTextSelection, ref, &loc); err != nil {
		return err
	}
	if loc.Start == nil || loc.End == nil {
		return errs.Validationf("sharetype: %q anchor_ref requires start and end offsets", AnchorTextSelection)
	}
	if *loc.Start < 0 || *loc.End <= *loc.Start {
		return errs.Validationf("sharetype: %q anchor_ref requires 0 <= start < end", AnchorTextSelection)
	}
	if loc.Quote == nil || *loc.Quote == "" {
		return errs.Validationf("sharetype: %q anchor_ref requires the quoted substring", AnchorTextSelection)
	}
	return nil
}

// codeLineLocator validates {"line":42} with an optional short "hash" of the
// line's text so a client can detect a stale render (ADR-0006, see
// internal/annotation.LineHash).
func codeLineLocator(ref json.RawMessage) error {
	var loc struct {
		Line *int    `json:"line"`
		Hash *string `json:"hash"`
	}
	if err := decodeStrict(AnchorCodeLine, ref, &loc); err != nil {
		return err
	}
	if loc.Line == nil || *loc.Line < 1 {
		return errs.Validationf("sharetype: %q anchor_ref requires line >= 1", AnchorCodeLine)
	}
	if loc.Hash != nil && *loc.Hash == "" {
		return errs.Validationf("sharetype: %q anchor_ref hash, when present, must be non-empty", AnchorCodeLine)
	}
	return nil
}

// codeRangeLocator validates {"start":40,"end":47} — an inclusive 1-based
// line range.
func codeRangeLocator(ref json.RawMessage) error {
	var loc struct {
		Start *int `json:"start"`
		End   *int `json:"end"`
	}
	if err := decodeStrict(AnchorCodeRange, ref, &loc); err != nil {
		return err
	}
	if loc.Start == nil || loc.End == nil {
		return errs.Validationf("sharetype: %q anchor_ref requires start and end lines", AnchorCodeRange)
	}
	if *loc.Start < 1 || *loc.End < *loc.Start {
		return errs.Validationf("sharetype: %q anchor_ref requires 1 <= start <= end", AnchorCodeRange)
	}
	return nil
}

// imageRegionLocator validates {"x":0.42,"y":0.31} with optional "w"/"h" for a
// box — normalized fractional coordinates in [0,1] so a pin survives display
// scaling and thumbnailing (ADR-0006 "Resolution-independent image pins").
func imageRegionLocator(ref json.RawMessage) error {
	var loc struct {
		X *float64 `json:"x"`
		Y *float64 `json:"y"`
		W *float64 `json:"w"`
		H *float64 `json:"h"`
	}
	if err := decodeStrict(AnchorImageRegion, ref, &loc); err != nil {
		return err
	}
	if loc.X == nil || loc.Y == nil {
		return errs.Validationf("sharetype: %q anchor_ref requires x and y", AnchorImageRegion)
	}
	inUnit := func(v float64) bool { return v >= 0 && v <= 1 }
	if !inUnit(*loc.X) || !inUnit(*loc.Y) {
		return errs.Validationf("sharetype: %q anchor_ref x/y must be normalized fractions in [0,1]", AnchorImageRegion)
	}
	if (loc.W == nil) != (loc.H == nil) {
		return errs.Validationf("sharetype: %q anchor_ref w and h must be supplied together", AnchorImageRegion)
	}
	if loc.W != nil && (!inUnit(*loc.W) || !inUnit(*loc.H)) {
		return errs.Validationf("sharetype: %q anchor_ref w/h must be normalized fractions in [0,1]", AnchorImageRegion)
	}
	return nil
}

// webhookRequestLocator validates {"request_id":"req_7Kx…"} — a capture-time
// id assigned once and never reissued (ADR-0010).
func webhookRequestLocator(ref json.RawMessage) error {
	var loc struct {
		RequestID *string `json:"request_id"`
	}
	if err := decodeStrict(AnchorWebhookRequest, ref, &loc); err != nil {
		return err
	}
	if loc.RequestID == nil || *loc.RequestID == "" {
		return errs.Validationf("sharetype: %q anchor_ref requires a non-empty request_id", AnchorWebhookRequest)
	}
	return nil
}

// spanIDLocator builds the {"span_id":"sp_a1…"} schema shared by the three
// trajectory anchors (turn, tool call, span) — all locate a capture-time
// span id (ADR-0009).
func spanIDLocator(anchor Anchor) LocatorFunc {
	return func(ref json.RawMessage) error {
		var loc struct {
			SpanID *string `json:"span_id"`
		}
		if err := decodeStrict(anchor, ref, &loc); err != nil {
			return err
		}
		if loc.SpanID == nil || *loc.SpanID == "" {
			return errs.Validationf("sharetype: %q anchor_ref requires a non-empty span_id", anchor)
		}
		return nil
	}
}
