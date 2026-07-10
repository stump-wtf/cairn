package annotation

// Anchor-stability primitives for text anchors.
//
// Anchors resolve reliably because their substrate is immutable — bodies are
// content-addressed and editing mints a new artifact (ADR-0008) — so the only
// remaining requirement is that render-time identifiers be deterministic:
// the same body must yield the same ids on every surface and every request.
// These helpers are that determinism, shared by ingest, the viewers
// (SPEC-0003/0005), and the annotation validator so an id can never drift
// between the surface that mints it and the anchor that stores it.
//
// Governing: ADR-0006 ("How anchors stay stable": deterministic render ids,
// self-describing selections), SPEC-0006 REQ "Anchor Stability".

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// blockIDHexLen is the number of hex digits a markdown block id carries after
// its "b_" prefix: 12 digits (48 bits) keeps collisions negligible within a
// single document while staying short enough for URLs and anchor_refs.
const blockIDHexLen = 12

// lineHashHexLen is the number of hex digits in a code line-text hash — a
// staleness detector, not an identifier, so 8 digits suffice.
const lineHashHexLen = 8

// BlockID derives the deterministic id of a markdown block from its structural
// position (0-based ordinal among the document's top-level blocks) and its
// content. The same body therefore renders the same block ids on every surface
// and every request (ADR-0006 "Deterministic render ids"), e.g. "b_3f2a9c81d04e".
// Content is used verbatim except for trailing whitespace, which markdown
// rendering does not preserve.
func BlockID(position int, content string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", position, strings.TrimRight(content, " \t\r\n"))))
	return "b_" + hex.EncodeToString(sum[:])[:blockIDHexLen]
}

// LineHash returns the short hash of a code line's text that a `code_line`
// anchor_ref may carry so a client can detect it is annotating against a
// stale render (ADR-0006). The line's trailing whitespace and line ending are
// ignored; leading whitespace is significant (indentation is code).
func LineHash(line string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(line, " \t\r\n")))
	return hex.EncodeToString(sum[:])[:lineHashHexLen]
}

// SelectionQuote extracts the substring a text_selection anchor quotes:
// body[start:end) in characters (runes, since offsets index the canonical
// body text, not its encoding). It returns ok=false when the offsets fall
// outside the body.
func SelectionQuote(body string, start, end int) (quote string, ok bool) {
	if start < 0 || end <= start {
		return "", false
	}
	runes := []rune(body)
	if end > len(runes) {
		return "", false
	}
	return string(runes[start:end]), true
}

// QuoteMatches reports whether a stored text_selection quote still matches the
// body at its offsets. A false result means the annotation must surface as
// "context changed" rather than mis-anchor (SPEC-0006 "Text selection quote
// mismatch") — a safeguard, not an expected case, given immutable bodies.
func QuoteMatches(body string, start, end int, quote string) bool {
	got, ok := SelectionQuote(body, start, end)
	return ok && got == quote
}
