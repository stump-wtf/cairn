package trajectory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// Governing: ADR-0025, SPEC-0019 VE-1, VE-4, VE-6

type wantViolation struct {
	field  string
	reason errs.Reason
}

func prepareFresh(spans ...SpanInput) error {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{}, spans)
	return err
}

func assertViolations(t *testing.T, err error, want ...wantViolation) []errs.Violation {
	t.Helper()
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("err = %v, want validation_failed", err)
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != len(want) {
		t.Fatalf("violations = %+v, want %d", vs, len(want))
	}
	for i, w := range want {
		if vs[i].Field != w.field || vs[i].Reason != w.reason || vs[i].Location != errs.LocBody {
			t.Fatalf("violation %d = %+v, want %s on %s", i, vs[i], w.reason, w.field)
		}
	}
	return vs
}

// VE-4: every structural problem in one batch is reported, in payload order,
// and the error still matches each sentinel it stands for.
func TestPrepareSpansReportsEveryProblem(t *testing.T) {
	err := prepareFresh(
		SpanInput{SpanID: "ok", Category: CategoryReason},
		SpanInput{SpanID: "blank", Category: "  "},
		SpanInput{SpanID: "orphan", ParentSpanID: "ghost", Category: CategoryExec},
		SpanInput{SpanID: "busy", Category: CategoryReason, Tool: "bash", DurationMS: -1},
	)
	vs := assertViolations(t, err,
		wantViolation{"spans[1].category", errs.ReasonRequired},
		wantViolation{"spans[2].parent_span_id", errs.ReasonNotAllowed},
		wantViolation{"spans[3].tool", errs.ReasonNotAllowed},
		wantViolation{"spans[3].duration_ms", errs.ReasonNotPositive},
	)
	if !errors.Is(err, ErrEmptyCategory) || !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("err = %v, want both ErrEmptyCategory and ErrUnknownParent in the chain", err)
	}
	if vs[1].Value == nil || *vs[1].Value != "ghost" {
		t.Fatalf("parent violation = %+v, want the unknown parent echoed", vs[1])
	}
	if !strings.HasPrefix(errs.Summary(vs), "4 problems: spans[1].category: ") {
		t.Fatalf("summary = %q", errs.Summary(vs))
	}
}

// A cycle names each member, not the spans that merely descend from it; a
// self-parent is named as such.
func TestPrepareSpansCycleNamesItsMembers(t *testing.T) {
	err := prepareFresh(
		SpanInput{SpanID: "a", ParentSpanID: "b", Category: CategoryReason},
		SpanInput{SpanID: "b", ParentSpanID: "a", Category: CategoryReason},
		SpanInput{SpanID: "c", ParentSpanID: "a", Category: CategoryReason},
		SpanInput{SpanID: "d", ParentSpanID: "d", Category: CategoryReason},
	)
	vs := assertViolations(t, err,
		wantViolation{"spans[0].parent_span_id", errs.ReasonNotAllowed},
		wantViolation{"spans[1].parent_span_id", errs.ReasonNotAllowed},
		wantViolation{"spans[3].parent_span_id", errs.ReasonNotAllowed},
	)
	if !strings.Contains(vs[0].Message, "cycle") || !strings.Contains(vs[2].Message, "its own parent") {
		t.Fatalf("messages = %q / %q", vs[0].Message, vs[2].Message)
	}
	if !errors.Is(err, ErrUnknownParent) || errors.Is(err, ErrEmptyCategory) {
		t.Fatalf("err = %v, want only ErrUnknownParent", err)
	}
}

// Missing and duplicate span_ids are reported per span, and duplicates do not
// stall the depth fixpoint.
func TestPrepareSpansSpanIDViolations(t *testing.T) {
	_, err := prepareSpans(map[string]int{"old": 0}, map[string]int{"": 1}, map[string]bool{"old": true}, []SpanInput{
		{SpanID: "", Category: CategoryReason},
		{SpanID: "old", Category: CategoryReason},
		{SpanID: "x", Category: CategoryReason},
		{SpanID: "x", ParentSpanID: "x", Category: CategoryReason},
	})
	assertViolations(t, err,
		wantViolation{"spans[0].span_id", errs.ReasonRequired},
		wantViolation{"spans[1].span_id", errs.ReasonDuplicate},
		wantViolation{"spans[3].span_id", errs.ReasonDuplicate},
		wantViolation{"spans[3].parent_span_id", errs.ReasonNotAllowed},
	)
}

func TestPrepareSpansFieldViolations(t *testing.T) {
	long := strings.Repeat("x", MaxCategoryLen+1)
	err := prepareFresh(
		SpanInput{SpanID: "s0", Category: Category(long)},
		SpanInput{SpanID: "s1", Category: CategoryRead, ProducedArtifactID: "abc", StartOffsetMS: -5},
	)
	vs := assertViolations(t, err,
		wantViolation{"spans[0].category", errs.ReasonTooLong},
		wantViolation{"spans[1].produced_artifact_id", errs.ReasonNotAllowed},
		wantViolation{"spans[1].start_offset_ms", errs.ReasonNotPositive},
	)
	if vs[0].Limit != MaxCategoryLen || vs[0].Unit != errs.UnitChars || vs[0].Value == nil || len(*vs[0].Value) != errs.MaxValueBytes {
		t.Fatalf("category violation = %+v, want a 64-char limit and a capped echo", vs[0])
	}
	if vs[2].Value == nil || *vs[2].Value != "-5" {
		t.Fatalf("timing violation = %+v, want -5 echoed", vs[2])
	}
}

// A batch far larger than the violation cap is bounded, not echoed whole.
func TestPrepareSpansViolationsBounded(t *testing.T) {
	spans := make([]SpanInput, errs.MaxViolations*3)
	for i := range spans {
		spans[i] = SpanInput{SpanID: "s" + strings.Repeat("x", i), Category: ""}
	}
	if vs := errs.ViolationsOf(prepareFresh(spans...)); len(vs) != errs.MaxViolations {
		t.Fatalf("violations = %d, want the cap of %d", len(vs), errs.MaxViolations)
	}
}

// VE-4: every oversize output in a batch is named, and the cap is checked
// before any output is staged. The service has no object store, so staging
// the first span's (spillable, in-cap) output would panic.
func TestSpanOutputCapNamesEveryOversizeSpan(t *testing.T) {
	svc := NewService(nil, nil, Options{InlineThresholdBytes: 2, MaxOutputBytes: 4})
	_, err := svc.AppendSpans(context.Background(), "x", "alice", []SpanInput{
		{SpanID: "s0", Category: CategoryExec, Output: []byte("abc")},
		{SpanID: "s1", Category: CategoryExec, Output: []byte("12345")},
		{SpanID: "s2", Category: CategoryExec, Output: []byte("123456")},
	})
	if errs.CodeOf(err) != errs.CodePayloadTooLarge || !errors.Is(err, errs.ErrTooLarge) {
		t.Fatalf("err = %v, want payload_too_large", err)
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 2 {
		t.Fatalf("violations = %+v, want spans 1 and 2", vs)
	}
	for i, v := range vs {
		if v.Field != spanField(i+1, "output") || v.Reason != errs.ReasonTooLarge || v.Limit != int64(4) || v.Unit != errs.UnitBytes || v.Value != nil {
			t.Fatalf("violation %d = %+v, want spans[%d].output too_large at 4 bytes, no echo", i, v, i+1)
		}
	}
}
