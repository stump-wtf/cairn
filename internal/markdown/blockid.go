// Package markdown renders a markdown artifact body into the sanitized,
// server-side HTML fragment the app shell drops into its body slot, together
// with the derived table of contents, the deterministic per-block ids anchors
// resolve against, and a small stats summary the metadata panel shows.
//
// Rendering is done entirely on the server and the output is sanitized so an
// untrusted body can never execute active content in Cairn's origin (SPEC-0003
// Security Requirements: "markdown MUST be sanitized"). goldmark itself omits
// raw HTML (no WithUnsafe), and bluemonday then strips anything a viewer must
// not carry, so the fragment is inert markup only.
//
// Governing: ADR-0011 (server-rendered viewer fragments, no build step),
// ADR-0006 (deterministic render ids make anchors durable), SPEC-0003 REQ
// "Markdown Viewer", REQ "Markdown Annotation Anchors", Security REQ.
package markdown

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

// BlockID derives the deterministic id of a markdown block from its structural
// position (0-based ordinal among the document's top-level blocks) and its
// content. The same body therefore renders the same block ids on every surface
// and every request (ADR-0006 "Deterministic render ids"), e.g.
// "b_3f2a9c81d04e", so a block reaction or comment survives re-render
// (SPEC-0003 REQ "Markdown Viewer": "Each rendered block MUST carry the
// deterministic block_id"). Content is used verbatim except for trailing
// whitespace, which markdown rendering does not preserve.
//
// This is the single source of truth for markdown block ids; the annotation
// stability layer delegates here so the id that mints a block can never drift
// from the id an anchor stores.
func BlockID(position int, content string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", position, strings.TrimRight(content, " \t\r\n"))))
	return "b_" + hex.EncodeToString(sum[:])[:blockIDHexLen]
}
