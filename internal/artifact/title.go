package artifact

import "github.com/stump-wtf/cairn/internal/errs"

// MaxTitleBytes bounds an artifact's display title. It is the bound the
// multipart create path always read a `title` field up to, so no title that
// path accepted is newly refused; what changes is that a longer title is
// rejected with the limit named, rather than silently cut (multipart) or
// stored whole (a header or query title).
const MaxTitleBytes = 4096

// CheckTitle reports a title over MaxTitleBytes as a too_long violation on the
// caller's own field and location, or nil. An empty title is valid: it is
// optional everywhere.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-3
func CheckTitle(title, field string, loc errs.Location) *errs.Invalid {
	if len(title) <= MaxTitleBytes {
		return nil
	}
	return errs.Violate(field, loc, errs.ReasonTooLong,
		errs.WithValue(title), errs.WithLimit(MaxTitleBytes, errs.UnitBytes))
}
