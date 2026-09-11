package artifact

import (
	"strings"

	"github.com/joestump/cairn/internal/errs"
)

// Tags are short, flat strings a creator attaches to an artifact so a
// downstream consumer can route it without reading the body — a Switchboard
// rule telling a handoff work order ("handoff") from an ordinary paste, and
// picking the lane that should run it ("lane:auto").
//
// Tags are client-asserted and are NOT provenance. ActorID and Channel are
// derived server-side from the authenticated surface; a tag is whatever the
// caller sent. A consumer MUST NOT base a trust or authorization decision on a
// tag: "handoff" says what the creator wants, never who the creator is. Trust
// comes from ActorID, the authenticated principal.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
// Tags"

// Tag bounds. They keep tags routing metadata rather than a second body: the
// whole list rides in every outbound event and is rendered in the web panel.
const (
	MaxTags     = 32
	MaxTagBytes = 64
)

// tagPunct is the punctuation a tag may use beside [a-z0-9]. ":" and "/" carry
// the handoff convention's "key:value" and "owner/name" shapes; "#" is admitted
// because its issue tag ("issue:stump.wtf/cairn#42") cannot be written without
// it. The comma is deliberately absent, so it is always safe as a list
// separator on the wire.
const tagPunct = "._:/#-"

// NormalizeTags validates raw tags and returns them deduplicated in
// first-occurrence order. A malformed tag rejects the whole list — nothing is
// truncated, lowercased or otherwise rewritten — and so does a list of more
// than MaxTags distinct tags; exact repeats are simply dropped. Nil or empty
// input yields nil.
func NormalizeTags(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, t := range raw {
		if err := ValidateTag(t); err != nil {
			return nil, err
		}
		if _, dup := seen[t]; dup {
			continue
		}
		if len(out) == MaxTags {
			return nil, errs.Validationf("tags: more than the maximum of %d distinct tags", MaxTags)
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

// ValidateTag checks one tag: 1 to MaxTagBytes bytes of lowercase [a-z0-9]
// and tagPunct. Uppercase is rejected rather than folded, so what a consumer
// matches is byte-for-byte what the creator sent.
func ValidateTag(t string) error {
	switch {
	case t == "":
		return errs.Validationf("tags: a tag must not be empty")
	case len(t) > MaxTagBytes:
		return errs.Validationf("tags: a tag is %d bytes, maximum is %d", len(t), MaxTagBytes)
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && strings.IndexByte(tagPunct, c) < 0 {
			return errs.Validationf("tags: tag %q may only contain lowercase [a-z0-9%s]", t, tagPunct)
		}
	}
	return nil
}

// validateNormalizedTags is the aggregate invariant: tags already normalized,
// so a persisted artifact can never hold a malformed or repeated tag.
func validateNormalizedTags(tags []string) error {
	norm, err := NormalizeTags(tags)
	if err != nil {
		return err
	}
	if len(norm) != len(tags) {
		return errs.Validationf("artifact: tags must not repeat")
	}
	return nil
}
