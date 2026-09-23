package artifact

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/cairn/internal/errs"
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
//
// Every failing tag is reported, each as a violation on tags[i] (the MCP
// argument's shape), so a caller fixes them all in one round trip.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-4
func NormalizeTags(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	var bad []*errs.Invalid
	tooMany := false
	for i, t := range raw {
		if inv := CheckTag(t, fmt.Sprintf("tags[%d]", i), errs.LocBody); inv != nil {
			bad = append(bad, inv)
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		if len(out) == MaxTags {
			tooMany = true
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if tooMany {
		bad = append(bad, TooManyTags("tags", errs.LocBody))
	}
	if inv := errs.Join(bad...); inv != nil {
		return nil, inv
	}
	return out, nil
}

// ValidateTag checks one tag: 1 to MaxTagBytes bytes of lowercase [a-z0-9]
// and tagPunct. Uppercase is rejected rather than folded, so what a consumer
// matches is byte-for-byte what the creator sent.
func ValidateTag(t string) error {
	if inv := CheckTag(t, "tag", errs.LocBody); inv != nil {
		return inv
	}
	return nil
}

// CheckTag is ValidateTag reporting a violation on the caller's own field and
// location, or nil when the tag is valid. A tag that is wrong only in its case
// is `uppercase`, so the fix it names is exact; any other disallowed character
// is `invalid_charset`.
//
// Governing: ADR-0018, ADR-0025, SPEC-0019 VE-1, VE-3
func CheckTag(t, field string, loc errs.Location) *errs.Invalid {
	switch {
	case t == "":
		return errs.Violate(field, loc, errs.ReasonRequired)
	case len(t) > MaxTagBytes:
		return errs.Violate(field, loc, errs.ReasonTooLong,
			errs.WithValue(t), errs.WithLimit(MaxTagBytes, errs.UnitBytes))
	}
	upper := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte(tagPunct, c) >= 0:
		case c >= 'A' && c <= 'Z':
			upper = true
		default:
			return errs.Violate(field, loc, errs.ReasonInvalidCharset,
				errs.WithValue(t), errs.WithExpect("lowercase a-z, 0-9 and "+tagPunct))
		}
	}
	if upper {
		return errs.Violate(field, loc, errs.ReasonUppercase, errs.WithValue(t))
	}
	return nil
}

// TooManyTags is the violation for a list of more than MaxTags distinct tags.
func TooManyTags(field string, loc errs.Location) *errs.Invalid {
	return errs.Violate(field, loc, errs.ReasonTooMany, errs.WithLimit(MaxTags, errs.UnitCount))
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
