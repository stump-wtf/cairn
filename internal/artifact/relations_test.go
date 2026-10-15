package artifact

import (
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: SPEC-0024 REQ "Relation Kinds and Limits"; ADR-0025, SPEC-0019 VE-1,
// VE-4.

func rel(kind RelationKind, id string) Relation {
	return Relation{Kind: kind, ID: id}
}

func TestNormalizeRelationsEmpty(t *testing.T) {
	for _, in := range [][]Relation{nil, {}} {
		out, err := NormalizeRelations(in)
		if err != nil || out != nil {
			t.Fatalf("NormalizeRelations(%v) = %v, %v; want nil, nil", in, out, err)
		}
	}
}

// Duplicate (kind, id) pairs are dropped keeping first-occurrence order; the
// same id under two kinds is two relations, not a duplicate.
func TestNormalizeRelationsDedupsKeepingOrder(t *testing.T) {
	in := []Relation{
		rel(RelationReplyTo, "aAAAaAAA"),
		rel(RelationFollows, "bBBBbBBB"),
		rel(RelationReplyTo, "aAAAaAAA"),
		rel(RelationReplyTo, "bBBBbBBB"),
	}
	out, err := NormalizeRelations(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []Relation{
		rel(RelationReplyTo, "aAAAaAAA"),
		rel(RelationFollows, "bBBBbBBB"),
		rel(RelationReplyTo, "bBBBbBBB"),
	}
	if len(out) != len(want) {
		t.Fatalf("got %d relations, want %d: %v", len(out), len(want), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("relations[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

func TestNormalizeRelationsRejectsBadKind(t *testing.T) {
	_, err := NormalizeRelations([]Relation{rel("parent", "aAAAaAAA")})
	if err == nil {
		t.Fatal("an undefined kind was accepted")
	}
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("code = %s, want validation_failed", errs.CodeOf(err))
	}
	var inv *errs.Invalid
	if !errors.As(err, &inv) || len(inv.Violations) != 1 {
		t.Fatalf("want one violation, got %v", err)
	}
	if got := inv.Violations[0].Reason; got != errs.ReasonUnknownValue {
		t.Errorf("reason = %q, want %q", got, errs.ReasonUnknownValue)
	}
	if inv.Violations[0].Field != "relations[0].kind" {
		t.Errorf("field = %q, want relations[0].kind", inv.Violations[0].Field)
	}
}

// Every malformed relation is reported at once, so a caller fixes them all in
// one round trip (SPEC-0019 VE-4).
func TestNormalizeRelationsReportsEveryFailure(t *testing.T) {
	_, err := NormalizeRelations([]Relation{
		rel("", "aAAAaAAA"),
		rel(RelationDerivedFrom, "not base62!"),
		rel(RelationFollows, ""),
	})
	var inv *errs.Invalid
	if !errors.As(err, &inv) {
		t.Fatalf("want a violation list, got %v", err)
	}
	if len(inv.Violations) != 3 {
		t.Fatalf("got %d violations, want 3: %v", len(inv.Violations), inv.Violations)
	}
}

func TestNormalizeRelationsCapsAtMax(t *testing.T) {
	in := make([]Relation, 0, MaxRelations+2)
	for i := range MaxRelations + 2 {
		in = append(in, rel(RelationFollows, string(rune('a'+i))+strings.Repeat("x", 7)))
	}
	_, err := NormalizeRelations(in)
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("9 relations accepted (err = %v)", err)
	}
	var inv *errs.Invalid
	if !errors.As(err, &inv) {
		t.Fatalf("want a violation list, got %v", err)
	}
	if inv.Violations[len(inv.Violations)-1].Reason != errs.ReasonTooMany {
		t.Errorf("last violation = %v, want too_many", inv.Violations[len(inv.Violations)-1])
	}
}

// A create never stores more than MaxRelations, and it never stores the eighth
// twice.
func TestNormalizeRelationsKeepsFirstMax(t *testing.T) {
	in := make([]Relation, 0, MaxRelations+3)
	for i := range MaxRelations {
		in = append(in, rel(RelationReplyTo, string(rune('a'+i))+strings.Repeat("y", 7)))
	}
	in = append(in, in[0], in[1], in[2])
	out, err := NormalizeRelations(in)
	if err != nil {
		t.Fatalf("duplicates past the cap should be dropped, not rejected: %v", err)
	}
	if len(out) != MaxRelations {
		t.Fatalf("got %d relations, want the cap %d", len(out), MaxRelations)
	}
}

func TestCheckRelationIDGrammar(t *testing.T) {
	good := []string{"a", "a1B2c3D4", strings.Repeat("aZ9", 10) + "ab"} // 32 bytes, the bound
	bad := []string{"", "with space", "under_score", "dash-ed", "dot.ted", strings.Repeat("a", 33)}
	for _, id := range good {
		if inv := CheckRelationID(id, "relation.id", errs.LocBody); inv != nil {
			t.Errorf("%q rejected: %v", id, inv)
		}
	}
	for _, id := range bad {
		if inv := CheckRelationID(id, "relation.id", errs.LocBody); inv == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

// The target-not-found error is one value for every kind of miss, so a create
// that fails gives a probe nothing to tell the cases apart by.
func TestRelationTargetNotFoundIsOneError(t *testing.T) {
	if !errors.Is(RelationTargetNotFound(), ErrRelationTargetNotFound) {
		t.Fatal("RelationTargetNotFound is not the sentinel")
	}
	if code := errs.CodeOf(RelationTargetNotFound()); code != errs.CodeValidation {
		t.Errorf("code = %s, want validation_failed", code)
	}
}

// The aggregate refuses an artifact holding an unnormalized relation list: a
// persisted artifact can never carry a duplicate or an undefined kind.
func TestValidateRejectsUnnormalizedRelations(t *testing.T) {
	a := validArtifact()
	a.Relations = []Relation{rel(RelationReplyTo, "bBBBbBBB"), rel(RelationReplyTo, "bBBBbBBB")}
	if err := a.Validate(); err == nil {
		t.Fatal("a repeated relation passed the aggregate invariant")
	}

	a.Relations = normalizeOrFail(t, []Relation{rel(RelationFollows, "bBBBbBBB")})
	if err := a.Validate(); err != nil {
		t.Fatalf("a normalized relation list must satisfy the invariant: %v", err)
	}
}

func normalizeOrFail(t *testing.T, in []Relation) []Relation {
	t.Helper()
	out, err := NormalizeRelations(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}
