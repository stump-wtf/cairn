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
	"strings"

	"github.com/stump-wtf/cairn/internal/markdown"
)

// blockIDHexLen is the number of hex digits a markdown block id carries after
// its "b_" prefix: 12 digits (48 bits) keeps collisions negligible within a
// single document while staying short enough for URLs and anchor_refs. It
// mirrors the markdown package's own constant so the anchor layer and the
// renderer agree on the id length.
const blockIDHexLen = 12

// lineHashHexLen is the number of hex digits in a code line-text hash — a
// staleness detector, not an identifier, so 8 digits suffice.
const lineHashHexLen = 8

// BlockID derives the deterministic id of a markdown block from its structural
// position and content. It delegates to the markdown renderer, which is the
// single source of truth for block ids, so the id that mints a block (the
// viewer) and the id an anchor stores (this layer) can never drift (ADR-0006
// "Deterministic render ids", SPEC-0006 REQ "Anchor Stability").
func BlockID(position int, content string) string {
	return markdown.BlockID(position, content)
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
