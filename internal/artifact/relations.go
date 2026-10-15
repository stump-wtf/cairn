package artifact

import (
	"slices"
	"strconv"

	"github.com/stump-wtf/cairn/internal/errs"
)

// A relation is one artifact naming another it replies to, derives from, or
// follows. Relations are declared at create and never edited: an edge written
// after the fact would rewrite the provenance record the artifact exists to
// keep (SPEC-0024 REQ "Relation Kinds and Limits").
//
// The kind set is deliberately closed and small. Each member answers a
// different question about why the link exists, and anything answerable by a
// tag is a tag, not a relation.

// RelationKind is the type of a relation between two artifacts.
type RelationKind string

const (
	RelationReplyTo     RelationKind = "reply_to"
	RelationDerivedFrom RelationKind = "derived_from"
	RelationFollows     RelationKind = "follows"
)

// relationKinds is the closed kind set, in the order a surface lists it.
var relationKinds = []RelationKind{RelationReplyTo, RelationDerivedFrom, RelationFollows}

// MaxRelations caps the relations one artifact may declare. The bound is what
// lets the read path return the whole set without paging it, and it keeps one
// create from turning the reverse lookup into a scan.
const MaxRelations = 8

// maxRelationIDBytes bounds a target public id before lookup. A minted id is
// far shorter, so anything longer is either a mistyped paste or an attempt to
// push a payload into a column sized for ids.
const maxRelationIDBytes = 32

// Relation is one declared link: the kind and the target's public id. The id is
// what the creator named; a read that cannot open the target returns neither it
// nor a title (SPEC-0024 REQ "Relations on Read").
type Relation struct {
	Kind RelationKind
	ID   string
}

// ErrRelationTargetNotFound is the single failure every unusable target
// produces: an id that does not exist, one whose artifact has expired, and one
// the creator cannot read all produce this error and nothing else. The
// distinction between those three is itself a probe answer, so the create that
// fails carries no id, no reason, and no hint of which case it was
// (SPEC-0024 REQ "Relation Targets Must Be Readable by the Creator").
//
// It is validation-coded, not a new violation reason: the wire shape stays the
// bare validation envelope, which is what makes the three responses
// byte-identical. A new reason would have to join SPEC-0019's closed registry
// and its public error reference before it could be emitted.
var ErrRelationTargetNotFound = errs.New(errs.CodeValidation, "relation target not found")

// RelationTargetNotFound is the error a create returns for a target it cannot
// use. It is always the same value: callers match it with errors.Is.
func RelationTargetNotFound() error { return ErrRelationTargetNotFound }

// NormalizeRelations validates declared relations and returns them deduplicated
// on (kind, id) in first-occurrence order, which is the order a read renders
// them in and the order their positions are stored in. Nothing is reordered,
// lowercased or truncated. Nil or empty input yields nil.
//
// Every failing relation is reported, each as a violation on relations[i], so a
// caller fixes them all in one round trip.
//
// Governing: SPEC-0024 REQ "Relation Kinds and Limits"; ADR-0025,
// SPEC-0019 VE-1, VE-4
func NormalizeRelations(raw []Relation) ([]Relation, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]Relation, 0, len(raw))
	seen := make(map[Relation]struct{}, len(raw))
	var bad []*errs.Invalid
	tooMany := false
	for i, r := range raw {
		if inv := CheckRelation(r, relationField(i), errs.LocBody); inv != nil {
			bad = append(bad, inv)
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		if len(out) == MaxRelations {
			tooMany = true
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if tooMany {
		bad = append(bad, TooManyRelations("relations", errs.LocBody))
	}
	if inv := errs.Join(bad...); inv != nil {
		return nil, inv
	}
	return out, nil
}

// ValidateRelation checks one relation: a kind from the closed set and a
// well-formed target id.
func ValidateRelation(r Relation) error {
	if inv := CheckRelation(r, "relation", errs.LocBody); inv != nil {
		return inv
	}
	return nil
}

// CheckRelation is ValidateRelation reporting a violation on the caller's own
// field and location, or nil when the relation is valid.
//
// Governing: SPEC-0024 REQ "Relation Kinds and Limits", REQ "Input Handling";
// ADR-0025
func CheckRelation(r Relation, field string, loc errs.Location) *errs.Invalid {
	if inv := CheckRelationKind(r.Kind, field+".kind", loc); inv != nil {
		return inv
	}
	return CheckRelationID(r.ID, field+".id", loc)
}

// CheckRelationKind validates a relation kind against the closed set.
func CheckRelationKind(k RelationKind, field string, loc errs.Location) *errs.Invalid {
	switch {
	case k == "":
		return errs.Violate(field, loc, errs.ReasonRequired)
	case !slices.Contains(relationKinds, k):
		return errs.Violate(field, loc, errs.ReasonUnknownValue,
			errs.WithValue(string(k)), errs.WithExpect("reply_to, derived_from or follows"))
	}
	return nil
}

// CheckRelationID validates a target id against the public-id grammar before
// anything looks it up: base62, within the length bound, and not a reserved
// route word (which no minted id can be). An id that fails here is reported
// precisely, which is safe — it names the caller's own typo, not the state of
// some other artifact.
//
// Governing: ADR-0005, SPEC-0024 REQ "Input Handling"
func CheckRelationID(id, field string, loc errs.Location) *errs.Invalid {
	switch {
	case id == "":
		return errs.Violate(field, loc, errs.ReasonRequired)
	case len(id) > maxRelationIDBytes:
		return errs.Violate(field, loc, errs.ReasonTooLong,
			errs.WithValue(id), errs.WithLimit(maxRelationIDBytes, errs.UnitBytes))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return errs.Violate(field, loc, errs.ReasonInvalidCharset,
			errs.WithValue(id), errs.WithExpect("base62"))
	}
	return nil
}

// TooManyRelations is the violation for a list of more than MaxRelations
// distinct relations.
func TooManyRelations(field string, loc errs.Location) *errs.Invalid {
	return errs.Violate(field, loc, errs.ReasonTooMany, errs.WithLimit(MaxRelations, errs.UnitCount))
}

// relationField names one relation in a list for its violation, the way
// tags[i] names one tag.
func relationField(i int) string {
	return "relations[" + strconv.Itoa(i) + "]"
}

// validateNormalizedRelations is the aggregate invariant: relations already
// normalized, so a persisted artifact can never hold a malformed or repeated
// relation, and never more than MaxRelations of them.
func validateNormalizedRelations(relations []Relation) error {
	norm, err := NormalizeRelations(relations)
	if err != nil {
		return err
	}
	if len(norm) != len(relations) {
		return errs.Validationf("artifact: relations must not repeat")
	}
	return nil
}
