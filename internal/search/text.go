package search

import (
	"mime"
	"slices"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
)

// maxCommentTextBytes bounds how much of an artifact's comment thread enters
// the document. It is not a config knob: comments are short by contract, and
// the bound exists so a pathological thread cannot out-grow the body it
// annotates (SPEC-0022 REQ-2 indexes them, and REQ-8 keeps them lexical-only,
// which is what stops them from being chunked into an embeddings request).
const maxCommentTextBytes = 64 << 10

// textualMedia are the non-`text/*` media types whose content is text. The list
// is deliberately identical to the ingest scanner's (internal/store/scan.go),
// because the scanner is what decides whether a body gets redacted at all
// (SPEC-0017 RD-6): index a body the scanner called binary and the indexer
// would be searching unscanned bytes.
var textualMedia = []string{
	"application/json", "application/xml", "application/yaml", "application/toml",
	"application/javascript", "application/x-sh",
}

// isTextualMedia reports whether a declared media type names text. Parameters
// (`text/csv;charset=utf-8`) and structured suffixes (`application/ld+json`)
// are handled the way the scanner handles them.
func isTextualMedia(mediaType string) bool {
	mt, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(mediaType))
	}
	mt = strings.ToLower(mt)
	return strings.HasPrefix(mt, "text/") || slices.Contains(textualMedia, mt) ||
		strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// indexesBody reports whether an artifact's own body contributes text to its
// document.
//
// The skip list is SPEC-0022 REQ-2's: webhook captures (their content is the
// captured-request stream, which holds other people's payloads and, for a
// public endpoint, whatever an attacker posts) and trace span payloads (a
// trajectory contributes its title and tags like any other artifact, never its
// spans) are never indexed. Neither has a body row at all — both share types
// are bodyless (ADR-0009, ADR-0010) — so this is a defense-in-depth predicate,
// not the only guard. Images and other binaries are excluded by media type.
func indexesBody(shareType artifact.ShareType, mediaType string) bool {
	switch shareType {
	case artifact.TypeWebhook, artifact.TypeTrajectory:
		return false
	}
	if strings.HasPrefix(strings.ToLower(string(mediaType)), "image/") {
		return false
	}
	return isTextualMedia(mediaType)
}

// memberIsText reports whether one bundle member contributes text. Bundle
// members carry their own media types, so each is decided on its own and each
// gets its own cap (SPEC-0022 REQ-2 "the text members of a bundle, under the
// same cap per member").
func memberIsText(mediaType string) bool {
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return false
	}
	return isTextualMedia(mediaType)
}

// cappedText trims s to at most limit bytes, never cutting inside a UTF-8
// sequence, so the tsvector and the snippet source stay valid text. A limit of
// 0 or less means unlimited.
func cappedText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
