// Unit coverage for the pure A2UI projection helpers — the flame graph's
// column math, category ranking/glyph assignment, and the bundle view's
// derived fields. These need no database: the store-backed paths are covered
// by mcp_a2ui_integration_test.go; what lives here is the geometry a broken
// clamp or rounding would silently ruin in a terminal.
package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
)

func TestA2UIRankCategories(t *testing.T) {
	ranked, glyphs := a2uiRankCategories(map[string]int64{
		"exec": 500, "read": 50, "net": 50,
	})
	if len(ranked) != 3 {
		t.Fatalf("ranked = %+v, want 3 entries", ranked)
	}
	if ranked[0].name != "exec" || ranked[0].glyph != '█' {
		t.Fatalf("top category = %+v, want exec with the densest glyph", ranked[0])
	}
	// Ties break alphabetically so output is deterministic: net before read.
	if ranked[1].name != "net" || ranked[2].name != "read" {
		t.Fatalf("tie order = %q, %q; want net then read", ranked[1].name, ranked[2].name)
	}
	if glyphs["exec"] != '█' || glyphs["net"] != '▓' || glyphs["read"] != '▒' {
		t.Fatalf("glyph map = %v, want palette order by rank", glyphs)
	}
}

func TestA2UIRankCategoriesFoldsOther(t *testing.T) {
	byCat := map[string]int64{
		"a": 700, "b": 600, "c": 500, "d": 400, "e": 300, "f": 20, "g": 10,
	}
	ranked, glyphs := a2uiRankCategories(byCat)
	if len(ranked) != len(a2uiCategoryPalette)+1 {
		t.Fatalf("ranked = %+v, want palette cap + one other bucket", ranked)
	}
	last := ranked[len(ranked)-1]
	if last.name != "other" || last.ms != 30 || last.glyph != a2uiGlyphOther {
		t.Fatalf("other bucket = %+v, want the folded 30ms remainder", last)
	}
	// Folded categories still resolve to a glyph for their flame bars.
	if glyphs["f"] != a2uiGlyphOther || glyphs["g"] != a2uiGlyphOther {
		t.Fatalf("folded glyphs = %v, want a2uiGlyphOther for f and g", glyphs)
	}
}

func TestA2UIFlameBar(t *testing.T) {
	// Full-range span fills the gutter.
	if got := a2uiFlameBar(0, 100, 100, '█'); got != strings.Repeat("█", a2uiFlameGutterW) {
		t.Fatalf("full bar = %q", got)
	}
	// A span in the second half starts past the midpoint.
	half := a2uiFlameBar(50, 50, 100, '▓')
	if len([]rune(half)) != a2uiFlameGutterW {
		t.Fatalf("bar = %q, want exactly %d cells", half, a2uiFlameGutterW)
	}
	if !strings.HasPrefix(half, strings.Repeat(" ", a2uiFlameGutterW/2)+"▓") {
		t.Fatalf("second-half bar = %q, want the bar to start at the midpoint", half)
	}
	// A tiny span still draws one cell.
	tiny := a2uiFlameBar(0, 1, 1_000_000, '█')
	if !strings.Contains(tiny, "█") {
		t.Fatalf("tiny span bar = %q, want at least one cell", tiny)
	}
	// Offsets past the wall clamp inside the gutter instead of overflowing.
	over := a2uiFlameBar(200, 500, 100, '█')
	if len([]rune(over)) != a2uiFlameGutterW {
		t.Fatalf("clamped bar = %q, want exactly %d cells", over, a2uiFlameGutterW)
	}
	// Negative offsets clamp to the left edge.
	neg := a2uiFlameBar(-50, 10, 100, '█')
	if len([]rune(neg)) != a2uiFlameGutterW || !strings.HasPrefix(neg, "█") {
		t.Fatalf("negative-offset bar = %q, want a left-edge bar", neg)
	}
}

func TestA2UIFlameAxis(t *testing.T) {
	axis := a2uiFlameAxis(500)
	if len([]rune(axis)) != a2uiFlameGutterW {
		t.Fatalf("axis = %q (%d runes), want exactly %d", axis, len([]rune(axis)), a2uiFlameGutterW)
	}
	if !strings.HasPrefix(axis, "0") || !strings.HasSuffix(axis, "500ms") {
		t.Fatalf("axis = %q, want 0 at the left and the wall time at the right", axis)
	}
}

func TestA2UIDistBar(t *testing.T) {
	ranked, _ := a2uiRankCategories(map[string]int64{"exec": 500, "read": 50})
	bar := a2uiDistBar(ranked, a2uiDistBarW)
	if got := len([]rune(bar)); got != a2uiDistBarW {
		t.Fatalf("dist bar = %q (%d runes), want exactly %d", bar, got, a2uiDistBarW)
	}
	if !strings.HasPrefix(bar, "█") || !strings.HasSuffix(bar, "▓") {
		t.Fatalf("dist bar = %q, want exec cells then read cells", bar)
	}
	if a2uiDistBar(nil, a2uiDistBarW) != "" {
		t.Fatal("empty ranking must render no bar")
	}
}

func TestA2UIPadLabel(t *testing.T) {
	if got := a2uiPadLabel("ab", 4); got != "ab  " {
		t.Fatalf("padded = %q", got)
	}
	if got := a2uiPadLabel("abcdef", 4); got != "abc…" {
		t.Fatalf("truncated = %q", got)
	}
	// Rune-aware: multibyte glyphs count as one cell each.
	if got := a2uiPadLabel("日本語のラベル", 4); got != "日本語…" {
		t.Fatalf("multibyte truncation = %q", got)
	}
}

func TestA2UIHotSpots(t *testing.T) {
	spans := []spanView{{
		Name: "root", DurationMS: 500,
		Children: []spanView{
			{Name: "fast child", DurationMS: 50},
			{Name: "container", DurationMS: 200,
				Children: []spanView{{Name: "leaf", DurationMS: 200}}},
		},
	}}
	hot := a2uiHotSpots(spans, a2uiMaxHotSpots)
	// root self = 500-250 = 250, leaf = 200, fast child = 50; the container
	// (200 - 200 = 0 self) must not appear.
	if len(hot) != 3 {
		t.Fatalf("hot = %+v, want 3 entries", hot)
	}
	if hot[0].name != "root" || hot[0].selfMS != 250 {
		t.Fatalf("hot[0] = %+v, want root at 250ms self", hot[0])
	}
	if hot[1].name != "leaf" || hot[2].name != "fast child" {
		t.Fatalf("hot order = %+v, want leaf then fast child", hot)
	}
	for _, h := range hot {
		if h.name == "container" {
			t.Fatal("zero-self container span must be skipped")
		}
	}
}

func TestA2UIRunWall(t *testing.T) {
	// A span ending past the recorded wall time widens the timeline.
	spans := []spanView{{StartOffsetMS: 400, DurationMS: 300}}
	if got := a2uiRunWall(statsView{WallTimeMS: 500}, spans); got != 700 {
		t.Fatalf("wall = %d, want 700 (widened to the trailing span)", got)
	}
	if got := a2uiRunWall(statsView{WallTimeMS: 900}, spans); got != 900 {
		t.Fatalf("wall = %d, want the recorded 900", got)
	}
}

func TestA2UISizeBar(t *testing.T) {
	if got := a2uiSizeBar(100, 100, 8); got != strings.Repeat("█", 8) {
		t.Fatalf("largest member bar = %q, want full", got)
	}
	if got := a2uiSizeBar(50, 100, 8); got != "████····" {
		t.Fatalf("half bar = %q", got)
	}
	// A tiny non-empty member still shows one cell.
	if got := a2uiSizeBar(1, 1_000_000, 8); got != "█·······" {
		t.Fatalf("tiny member bar = %q, want a single cell", got)
	}
	if got := a2uiSizeBar(0, 100, 8); got != strings.Repeat("·", 8) {
		t.Fatalf("empty member bar = %q, want all track", got)
	}
}

func TestA2UILabelSanitizer(t *testing.T) {
	in := "run `go test` | step\n**bold**"
	got := a2uiLabelSanitizer.Replace(in)
	for _, banned := range []string{"\n", "|", "`", "**"} {
		if strings.Contains(got, banned) {
			t.Fatalf("sanitized label %q still contains %q", got, banned)
		}
	}
}

// TestA2UIBundleViewDerivedFields exercises the pure bundle projection: the
// header totals, the badge mix ordering, and the per-member engagement
// segments that only render when non-zero.
func TestA2UIBundleViewDerivedFields(t *testing.T) {
	art := &artifact.Artifact{
		Title:     "audit drop",
		ExpiresAt: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		Access:    artifact.AccessPolicy{Visibility: artifact.VisibilityLink},
	}
	members := []a2uiMemberView{
		{Name: "report.md", Size: 100, MediaType: "text/markdown", Badge: "MD", Comments: 2, Reactions: 1},
		{Name: "main.go", Size: 50, MediaType: "text/x-go", Badge: "CODE"},
		{Name: "notes.md", Size: 25, MediaType: "text/markdown", Badge: "MD"},
	}
	idx := a2uiIndex(a2uiBundleView("b1", art, members))

	meta, _ := idx["hdr-meta"]["text"].(string)
	if !strings.Contains(meta, "3 members") || !strings.Contains(meta, "175 B total") {
		t.Fatalf("hdr-meta = %q, want count and summed size", meta)
	}
	mix, _ := idx["hdr-mix"]["text"].(string)
	if mix != "2 MD · 1 CODE" {
		t.Fatalf("hdr-mix = %q, want most-common-first badge counts", mix)
	}
	m0, _ := idx["member-0-meta"]["text"].(string)
	if !strings.Contains(m0, "💬 2") || !strings.Contains(m0, "♥ 1") {
		t.Fatalf("member-0-meta = %q, want engagement segments", m0)
	}
	if !strings.Contains(m0, strings.Repeat("█", a2uiSizeBarW)) {
		t.Fatalf("member-0-meta = %q, want a full size bar for the largest member", m0)
	}
	m1, _ := idx["member-1-meta"]["text"].(string)
	if strings.Contains(m1, "💬") || strings.Contains(m1, "♥") {
		t.Fatalf("member-1-meta = %q, engagement must be omitted when zero", m1)
	}
}
