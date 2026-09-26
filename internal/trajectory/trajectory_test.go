package trajectory

import (
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/sharetype"
)

// TestRegistryEntry pins the trajectory's registry affordances (SPEC-0004
// "Trajectory Annotation Anchors", ADR-0005 URL scheme): badge TRC and the
// reader-facing name "trace" (ADR-0016), the run/ URL + MCP prefix kept because
// a trace is OF a run, reactions on turn/toolcall, comments on
// span/text_selection.
func TestRegistryEntry(t *testing.T) {
	reg := sharetype.Default()
	h := reg.Resolve(sharetype.KeyTrajectory)
	if h.Badge() != "TRC" {
		t.Errorf("badge = %q, want TRC", h.Badge())
	}
	if pref := reg.URLPrefixFor(sharetype.KeyTrajectory); pref.Web != "run" || pref.MCP != "run" {
		t.Errorf("URL prefix = %+v, want run/run", pref)
	}
	// Reactions on turn + toolcall; comments on span + text_selection.
	cases := []struct {
		anchor sharetype.Anchor
		kind   sharetype.AnnotationKind
		want   bool
	}{
		{sharetype.AnchorTrajectoryTurn, sharetype.KindReaction, true},
		{sharetype.AnchorTrajectoryToolCall, sharetype.KindReaction, true},
		{sharetype.AnchorTrajectorySpan, sharetype.KindComment, true},
		{sharetype.AnchorTextSelection, sharetype.KindComment, true},
		{sharetype.AnchorTrajectorySpan, sharetype.KindReaction, false},
		{sharetype.AnchorTrajectoryTurn, sharetype.KindComment, false},
	}
	for _, c := range cases {
		if got := reg.AllowsAnchor(sharetype.KeyTrajectory, c.anchor, c.kind); got != c.want {
			t.Errorf("AllowsAnchor(%s,%s) = %v, want %v", c.anchor, c.kind, got, c.want)
		}
	}
}

// TestPrepareSpansDerivesDepthAndSeq proves the pure tree preparation resolves
// depth from the parent chain and assigns a gap-free sibling seq in input order,
// regardless of how the payload is ordered.
func TestPrepareSpansDerivesDepthAndSeq(t *testing.T) {
	// Children supplied BEFORE their parent to exercise the fixpoint resolver.
	news := []SpanInput{
		{SpanID: "s6a", ParentSpanID: "s6", Category: CategoryNet, Tool: "web_search"},
		{SpanID: "s1", Category: CategoryReason},
		{SpanID: "s6", Category: CategoryNet, Tool: "sub-agent"},
		{SpanID: "s6b", ParentSpanID: "s6", Category: CategoryNet, Tool: "web_fetch"},
		{SpanID: "s2", Category: CategoryExec, Tool: "bash"},
	}
	prepared, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{}, news)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	byID := map[string]preparedSpan{}
	for _, p := range prepared {
		byID[p.in.SpanID] = p
	}
	// Depths.
	for id, want := range map[string]int{"s1": 0, "s2": 0, "s6": 0, "s6a": 1, "s6b": 1} {
		if got := byID[id].depth; got != want {
			t.Errorf("span %s depth = %d, want %d", id, got, want)
		}
	}
	// Top-level seq follows input order among top-level spans: s1(0), s6(1), s2(2).
	for id, want := range map[string]int{"s1": 0, "s6": 1, "s2": 2} {
		if got := byID[id].seq; got != want {
			t.Errorf("top-level span %s seq = %d, want %d", id, got, want)
		}
	}
	// s6's children seq follows input order: s6a(0), s6b(1).
	for id, want := range map[string]int{"s6a": 0, "s6b": 1} {
		if got := byID[id].seq; got != want {
			t.Errorf("child span %s seq = %d, want %d", id, got, want)
		}
	}
}

// TestPrepareSpansAppendContinuesSeq proves an append continues the sibling seq
// from the already-persisted children rather than restarting at 0.
func TestPrepareSpansAppendContinuesSeq(t *testing.T) {
	existingDepth := map[string]int{"s1": 0, "s6": 0}
	existingChildCount := map[string]int{"": 2, "s6": 1} // 2 top-level, 1 child under s6
	existingIDs := map[string]bool{"s1": true, "s6": true, "s6a": true}

	news := []SpanInput{
		{SpanID: "s2", Category: CategoryReason},                                      // top-level -> seq 2
		{SpanID: "s6b", ParentSpanID: "s6", Category: CategoryNet, Tool: "web_fetch"}, // child of s6 -> seq 1
	}
	prepared, err := prepareSpans(existingDepth, existingChildCount, existingIDs, news)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared[0].seq != 2 {
		t.Errorf("appended top-level seq = %d, want 2 (continues 2 persisted)", prepared[0].seq)
	}
	if prepared[1].depth != 1 || prepared[1].seq != 1 {
		t.Errorf("appended child depth/seq = %d/%d, want 1/1", prepared[1].depth, prepared[1].seq)
	}
}

// The category set is OPEN (ADR-0009, SPEC-0004 "Non-recommended category
// accepted"): a value outside the recommended thirteen is accepted and rendered
// neutrally, never rejected, so agents are not forced to remap their vocabulary.
// This test used to assert the opposite — the closed-enum behaviour PR #2
// superseded in the docs without the code following (issue #5).
func TestPrepareSpansUnrecommendedCategoryAccepted(t *testing.T) {
	for _, cat := range []Category{"search", "investigation", "review", "deploy", "wat"} {
		prepared, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
			[]SpanInput{{SpanID: "s1", Category: cat}})
		if err != nil {
			t.Fatalf("category %q: %v, want accepted", cat, err)
		}
		if prepared[0].in.Category != cat {
			t.Errorf("category %q persisted as %q, want it stored verbatim", cat, prepared[0].in.Category)
		}
	}
}

// Open does not mean unvalidated: empty, whitespace-only, and over-long
// categories are still refused (SPEC-0004 "Empty or missing category rejected",
// "Category length bounded").
//
// The U+00A0 cases pin the one place the two enforcement layers deliberately
// disagree. Go's unicode.IsSpace treats a non-breaking space as whitespace, so
// TrimSpace reduces these to "" and the service refuses them; Postgres' POSIX
// `\S` does not, so 0013's CHECK would accept the same value. That asymmetry is
// safe in exactly one direction — the service must be the STRICTER layer, with
// the CHECK as a backstop that never sees the value. If validation is ever
// loosened to a bare `!= ""`, these cases fail and say so, rather than letting
// an invisible-only category reach a legend that renders as blank.
func TestPrepareSpansEmptyOrOverLongCategoryRejected(t *testing.T) {
	for name, cat := range map[string]Category{
		"empty":            "",
		"whitespace":       "   ",
		"tab and newline":  "\t\n ",
		"over max len":     Category(strings.Repeat("x", MaxCategoryLen+1)),
		"padded to over":   Category(" " + strings.Repeat("x", MaxCategoryLen) + " "),
		"non-breaking sp":  " ",
		"nbsp and newline": " \n",
	} {
		_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
			[]SpanInput{{SpanID: "s1", Category: cat}})
		if !errors.Is(err, ErrEmptyCategory) {
			t.Errorf("%s: err = %v, want ErrEmptyCategory", name, err)
		}
	}
	// Exactly at the ceiling is fine — the bound is inclusive.
	if _, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: Category(strings.Repeat("x", MaxCategoryLen))}}); err != nil {
		t.Errorf("category of exactly MaxCategoryLen: %v, want accepted", err)
	}

	// The ceiling counts characters, not bytes, so it means the same thing as the
	// schema's length(category) <= 64. A byte count would reject this — 64
	// characters, 192 bytes — while the database accepted it, and the two layers
	// would disagree about the very same value.
	multibyte := Category(strings.Repeat("調", MaxCategoryLen))
	if len(multibyte) <= MaxCategoryLen {
		t.Fatalf("fixture is not multibyte: %d bytes for %d runes", len(multibyte), MaxCategoryLen)
	}
	if _, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: multibyte}}); err != nil {
		t.Errorf("64-character multibyte category: %v, want accepted (the bound counts runes)", err)
	}
}

// An unrecommended category may carry a tool: with an open set we cannot know
// which of an agent's own categories are tool-shaped, and the record only ever
// says `reason` takes none (ADR-0009).
func TestPrepareSpansToolOnUnrecommendedCategoryAccepted(t *testing.T) {
	if _, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: "deploy", Tool: "kubectl"}}); err != nil {
		t.Fatalf("tool on an unrecommended category: %v, want accepted", err)
	}
}

func TestPrepareSpansUnknownParent(t *testing.T) {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s2", ParentSpanID: "ghost", Category: CategoryReason}})
	if !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("err = %v, want ErrUnknownParent", err)
	}
}

func TestPrepareSpansCycleIsUnknownParent(t *testing.T) {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{
			{SpanID: "a", ParentSpanID: "b", Category: CategoryReason},
			{SpanID: "b", ParentSpanID: "a", Category: CategoryReason},
		})
	if !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("err = %v, want ErrUnknownParent for a 2-cycle", err)
	}
}

func TestPrepareSpansToolOnReasonRejected(t *testing.T) {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: CategoryReason, Tool: "bash"}})
	if err == nil {
		t.Fatal("a reason span carrying a tool must be rejected")
	}
}

func TestPrepareSpansProducedOnNonWriteRejected(t *testing.T) {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: CategoryRead, Tool: "read", ProducedArtifactID: "abc"}})
	if err == nil {
		t.Fatal("a non-write span declaring a produced artifact must be rejected")
	}
}

// TestNormalizeArtifactID pins the handle-form resolution added for issue #50:
// a produced_artifact_id supplied as an mcp://cairn/<id> handle (optionally
// under a /run/ or /hook/ sub-prefix a caller may copy-paste) must normalize to
// the bare public id the produced-edge lookup takes, identical to a bare id.
func TestNormalizeArtifactID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"TE4Fb89R", "TE4Fb89R"},
		{" mcp://cairn/TE4Fb89R ", "TE4Fb89R"},
		{"mcp://cairn/run/TE4Fb89R", "TE4Fb89R"},
		{"mcp://cairn/hook/TE4Fb89R", "TE4Fb89R"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeArtifactID(c.in); got != c.want {
			t.Errorf("normalizeArtifactID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPrepareSpansNormalizesProducedHandle pins that the handle is normalized at
// ingest rather than only at the produced-edge lookup: a prepared span feeds the
// span projected back to an appending caller AND the span published to live SSE
// subscribers, both of which would otherwise render a cross-link to
// `/mcp://cairn/<id>` until the viewer reloaded and re-read the bare id.
func TestPrepareSpansNormalizesProducedHandle(t *testing.T) {
	out, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{{SpanID: "s1", Category: CategoryWrite, ProducedArtifactID: "mcp://cairn/TE4Fb89R"}})
	if err != nil {
		t.Fatalf("prepareSpans: %v", err)
	}
	if got := out[0].in.ProducedArtifactID; got != "TE4Fb89R" {
		t.Errorf("prepared produced id = %q, want the bare id TE4Fb89R", got)
	}
}

func TestPrepareSpansDuplicateID(t *testing.T) {
	_, err := prepareSpans(map[string]int{}, map[string]int{}, map[string]bool{},
		[]SpanInput{
			{SpanID: "s1", Category: CategoryReason},
			{SpanID: "s1", Category: CategoryReason},
		})
	if err == nil {
		t.Fatal("duplicate span_id in a payload must be rejected")
	}
}

func TestBuildTreeNestsAndOrders(t *testing.T) {
	flat := []*Span{
		{SpanID: "s6b", ParentSpanID: "s6", Seq: 1},
		{SpanID: "s1", Seq: 0},
		{SpanID: "s6", Seq: 1},
		{SpanID: "s6a", ParentSpanID: "s6", Seq: 0},
	}
	roots := buildTree(flat)
	if len(roots) != 2 || roots[0].SpanID != "s1" || roots[1].SpanID != "s6" {
		t.Fatalf("roots = %+v, want [s1 s6] in seq order", roots)
	}
	s6 := roots[1]
	if len(s6.Children) != 2 || s6.Children[0].SpanID != "s6a" || s6.Children[1].SpanID != "s6b" {
		t.Fatalf("s6 children = %+v, want [s6a s6b] in seq order", s6.Children)
	}
}

func TestComputeStats(t *testing.T) {
	flat := []*Span{
		{Category: CategoryReason, DurationMS: 2223},
		{Category: CategoryExec, Tool: "bash", DurationMS: 2907},
		{Category: CategoryNet, Tool: "sub-agent", DurationMS: 10602},
		{Category: CategoryReason, DurationMS: 2736},
	}
	st := computeStats(flat, 48100, 34200)
	if st.SpanCount != 4 {
		t.Errorf("span count = %d, want 4", st.SpanCount)
	}
	if st.ToolCallCount != 2 {
		t.Errorf("tool-call count = %d, want 2", st.ToolCallCount)
	}
	if st.WallTimeMS != 34200 || st.TokenCount != 48100 {
		t.Errorf("wall/tokens = %d/%d, want 34200/48100", st.WallTimeMS, st.TokenCount)
	}
	if st.TimeByCategoryMS[CategoryReason] != 4959 {
		t.Errorf("reason ms = %d, want 4959", st.TimeByCategoryMS[CategoryReason])
	}
	if st.TimeByCategoryMS[CategoryNet] != 10602 {
		t.Errorf("net ms = %d, want 10602", st.TimeByCategoryMS[CategoryNet])
	}
}

func TestMaxSpanEndMS(t *testing.T) {
	flat := []*Span{
		{StartOffsetMS: 0, DurationMS: 2223},
		{StartOffsetMS: 31464, DurationMS: 2736},  // ends 34200
		{StartOffsetMS: 10602, DurationMS: 10602}, // ends 21204
	}
	if got := maxSpanEndMS(flat); got != 34200 {
		t.Fatalf("max end = %d, want 34200", got)
	}
}
