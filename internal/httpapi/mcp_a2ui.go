// A2UI (Agent-to-UI) resources: render `application/a2ui+json` projections of
// a trajectory run's waterfall + stream, and a bundle's member cards, so an
// A2UI-capable MCP host (Crush once joestump-agent/crush#217 lands) draws the
// view inline instead of asking the model to re-render raw JSON.
//
// Two resources ship in v1, both read-only at the A2UI layer (mutations stay
// on the existing MCP tools; the buttons on these surfaces are placeholders
// until the `a2ui_action` round-trip lands in joestump-agent/crush#221):
//
//	cairn://run/{id}/a2ui       — trace header + stats (category bar, hot
//	                              spots) + a span flame graph: per-span
//	                              timeline bars in a fixed-width gutter
//	cairn://bundle/{id}/a2ui    — bundle envelope (totals, type mix) + a
//	                              member list with badges, relative size
//	                              bars and engagement counts
//	cairn://artifact/{id}/a2ui   — single-body artifact (markdown, code, file)
//
// Both follow the A2UI-over-MCP transport contract:
// https://a2ui.org/guides/a2ui_over_mcp/. The wire shape is the same
// `{"version":"v0.9","updateComponents":{...}}` envelope the Crush inline
// <a2ui-json> scanner consumes, so the payload is directly spliceable into
// a chat reply.
//
// Governing: joestump-agent/crush#217 (A2UI-over-MCP epic), issue #90
// (server-side story), ADR-0003 (thin adapter — these handlers resolve the
// same core reads the REST/JSON handlers do and only re-project the result).
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/trajectory"
)

// a2uiMIME is the registered media type for A2UI payloads over MCP (per
// https://a2ui.org/guides/a2ui_over_mcp/).
const a2uiMIME = "application/a2ui+json"

// a2uiCatalog identifies the standard A2UI catalog this server renders
// against. Crush renders this catalog natively; a host that doesn't know the
// catalog should refuse rather than guess.
const a2uiCatalog = "https://a2ui.dev/specification/0.9/standard_catalog_definition.json"

// a2uiVersion is the wire envelope version Crush's <a2ui-json> scanner
// expects at the top of an inline block; carried here so the resource and
// the inline block are byte-compatible.
const a2uiVersion = "v0.9"

// a2uiAudienceUser marks the resource annotations so hosts know the payload
// is meant for the human's view, not for the model to paraphrase. The
// underlying JSON remains available via the existing resources for the
// model's use.
var a2uiAudienceUser = &mcp.Annotations{Audience: []mcp.Role{"user"}}

// --- envelope -----------------------------------------------------------------

// a2uiEnvelope is the top-level shape the Crush <a2ui-json> scanner consumes.
// Reading one of these resources returns exactly this object as the resource
// contents, MIME `application/a2ui+json`.
type a2uiEnvelope struct {
	Version          string               `json:"version"`
	UpdateComponents a2uiUpdateComponents `json:"updateComponents"`
}

type a2uiUpdateComponents struct {
	SurfaceID  string          `json:"surfaceId"`
	CatalogID  string          `json:"catalogId"`
	Components []a2uiComponent `json:"components"`
}

// a2uiComponent is the loose shape every catalog component shares. The
// standard catalog schemas pin `component` to a const per type, but they all
// serialise into the same flat object — so the builders below construct a
// map and let the JSON encoder emit whatever fields each type needs. This is
// deliberately not a tagged-union: the catalog grows components over time
// and a closed type would need a new struct per addition.
type a2uiComponent = map[string]any

// a2uiText builds a Text component (variant matches the A2UI v0.9 standard
// catalog field name; the schema uses unevaluatedProperties: false, so a
// non-existent field like usageHint would be silently dropped by a compliant
// validator and the component would render without its intended style).
func a2uiText(id, text, hint string) a2uiComponent {
	c := a2uiComponent{"id": id, "component": "Text", "text": text}
	if hint != "" {
		c["variant"] = hint
	}
	return c
}

// a2uiCard wraps a single child in a Card. The child must be a container id
// (Column/Row/List) per the catalog's "do NOT pass multiple IDs" rule.
func a2uiCard(id, childID string) a2uiComponent {
	return a2uiComponent{"id": id, "component": "Card", "child": childID}
}

// a2uiColumn packs children vertically.
func a2uiColumn(id string, children []string) a2uiComponent {
	return a2uiComponent{"id": id, "component": "Column", "children": children}
}

// a2uiRow packs children horizontally.
func a2uiRow(id string, children []string) a2uiComponent {
	return a2uiComponent{"id": id, "component": "Row", "children": children}
}

// a2uiList packs children as a List (one item per row, divider between).
func a2uiList(id string, children []string) a2uiComponent {
	return a2uiComponent{"id": id, "component": "List", "children": children}
}

// a2uiDivider emits a horizontal rule.
func a2uiDivider(id string) a2uiComponent {
	return a2uiComponent{"id": id, "component": "Divider"}
}

// a2uiButton emits a labelled Button with an action name. The action payload
// is opaque to the catalog; the receiving host wires it back to the server
// (the a2ui_action round-trip — joestump-agent/crush#221).
func a2uiButton(id, labelID, actionName string, actionContext map[string]any) a2uiComponent {
	action := map[string]any{"name": actionName}
	if len(actionContext) > 0 {
		action["context"] = actionContext
	}
	return a2uiComponent{
		"id": id, "component": "Button",
		"child": labelID, "action": action,
	}
}

// --- shared rendering helpers -------------------------------------------------

// a2uiMS turns a duration in milliseconds into a short human-readable label.
// Zero and negative values render as "0ms" — a run with no wall time yet is
// honest, not missing data.
func a2uiMS(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%.1fm", float64(ms)/60_000)
	default:
		return fmt.Sprintf("%.1fh", float64(ms)/3_600_000)
	}
}

// a2uiHumanBytes renders a byte count the way the existing web UI does —
// plain integer up to a KiB, then one decimal of the largest unit that fits.
func a2uiHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// a2uiIndent prefixes a span label with two non-breaking spaces per depth
// level so the waterfall reads as a tree without needing a nested component
// per level. The catalog has no indent component, so the prefix is the
// portable encoding.
func a2uiIndent(depth int) string {
	if depth <= 0 {
		return ""
	}
	return strings.Repeat("  ", depth)
}

// --- flame graph helpers ------------------------------------------------------

// Column geometry for the trace flame graph. The server cannot know the
// host's terminal width, so rows are built to a fixed budget conservative
// enough to survive an 80-column host: label (26) + gutter edges (2) +
// gutter (28) + duration (~7) ≈ 64 cells before card chrome.
const (
	a2uiFlameLabelW   = 26 // rune width of the span-label column
	a2uiFlameGutterW  = 28 // rune width of the timeline gutter between ▕ ▏
	a2uiFlameMaxDepth = 6  // indent cap so deep trees don't consume the label
	a2uiDistBarW      = 24 // width of the stats time-by-category bar
	a2uiMaxHotSpots   = 3  // slowest-self-time spans named in the stats card
)

// a2uiCategoryPalette maps time-share rank onto a bar glyph, densest first,
// so the category that dominates the run draws the boldest bars. Categories
// are an OPEN set (trajectory.Category), so ranking — not a fixed name→glyph
// table — is what keeps any vocabulary renderable. Its length is also the
// legend cap: categories past it fold into one "other" bucket.
var a2uiCategoryPalette = []rune{'█', '▓', '▒', '░', '▞'}

// a2uiGlyphOther marks categories folded past the palette.
const a2uiGlyphOther = '▪'

// a2uiLabelSanitizer strips the characters that would break a flame row: a
// newline splits the row across lines, and "|", backticks and "**" can trip
// a host's markdown detection (a2tea re-renders body Text that looks like
// markdown, destroying column alignment). Lookalike glyphs keep the label
// readable.
var a2uiLabelSanitizer = strings.NewReplacer(
	"\n", " ", "\r", " ", "\t", " ",
	"|", "¦", "`", "'", "**", "∗∗",
)

// a2uiCatShare is one category's share of the run's summed span time, with
// its assigned bar glyph.
type a2uiCatShare struct {
	name  string
	ms    int64
	glyph rune
}

// a2uiRankCategories orders time-by-category descending (ties break on name
// so the output is deterministic), assigns palette glyphs to the top
// entries, and folds the remainder into one synthetic "other" entry. The
// returned map looks up the bar glyph for any category the run used —
// including the folded ones, which all draw a2uiGlyphOther.
func a2uiRankCategories(byCat map[string]int64) ([]a2uiCatShare, map[string]rune) {
	ranked := make([]a2uiCatShare, 0, len(byCat))
	for name, ms := range byCat {
		ranked = append(ranked, a2uiCatShare{name: name, ms: ms})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].ms != ranked[j].ms {
			return ranked[i].ms > ranked[j].ms
		}
		return ranked[i].name < ranked[j].name
	})
	glyphs := make(map[string]rune, len(ranked))
	if len(ranked) > len(a2uiCategoryPalette) {
		var otherMS int64
		for _, c := range ranked[len(a2uiCategoryPalette):] {
			otherMS += c.ms
			glyphs[c.name] = a2uiGlyphOther
		}
		ranked = append(ranked[:len(a2uiCategoryPalette)],
			a2uiCatShare{name: "other", ms: otherMS, glyph: a2uiGlyphOther})
	}
	for i := range ranked {
		if ranked[i].glyph == 0 {
			ranked[i].glyph = a2uiCategoryPalette[i]
		}
		glyphs[ranked[i].name] = ranked[i].glyph
	}
	return ranked, glyphs
}

// a2uiPadLabel pads (or truncates with an ellipsis) a label to exactly w
// display runes so every flame row's gutter starts in the same column.
func a2uiPadLabel(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}

// a2uiFlameBar draws one span's gutter: glyph cells covering the span's
// [start, start+duration) range scaled onto a2uiFlameGutterW cells. Every
// span draws at least one cell — a 3ms tool call is a real event, not
// invisible — and the bar is clamped inside the gutter whatever the offsets
// claim.
func a2uiFlameBar(startMS, durMS int, wallMS int64, glyph rune) string {
	gw := int64(a2uiFlameGutterW)
	start := 0
	if wallMS > 0 {
		start = int(int64(startMS) * gw / wallMS)
	}
	if start < 0 {
		start = 0
	}
	if start > a2uiFlameGutterW-1 {
		start = a2uiFlameGutterW - 1
	}
	width := int(int64(durMS) * gw / max(wallMS, 1))
	if width < 1 {
		width = 1
	}
	if start+width > a2uiFlameGutterW {
		width = a2uiFlameGutterW - start
	}
	return strings.Repeat(" ", start) +
		strings.Repeat(string(glyph), width) +
		strings.Repeat(" ", a2uiFlameGutterW-start-width)
}

// a2uiFlameAxis draws the timeline ruler the bars hang under: zero at the
// left edge, the run's wall time at the right.
func a2uiFlameAxis(wallMS int64) string {
	right := a2uiMS(wallMS)
	fill := a2uiFlameGutterW - 1 - len([]rune(right))
	if fill < 1 {
		fill = 1
	}
	return "0" + strings.Repeat("┄", fill) + right
}

// a2uiDistBar renders the ranked category shares as one fixed-width stacked
// bar. Cell counts come from cumulative rounding so they always sum to the
// bar width exactly; a category too small for a cell of its own disappears
// into its neighbour rather than inflating the bar.
func a2uiDistBar(ranked []a2uiCatShare, width int) string {
	var total int64
	for _, c := range ranked {
		total += c.ms
	}
	if total <= 0 {
		return ""
	}
	var b strings.Builder
	var cum int64
	cells := 0
	for _, c := range ranked {
		cum += c.ms
		next := int(cum * int64(width) / total)
		if n := next - cells; n > 0 {
			b.WriteString(strings.Repeat(string(c.glyph), n))
			cells = next
		}
	}
	return b.String()
}

// a2uiHotSpot is a span ranked by self time — its duration minus its
// children's, the flame-graph notion of where the run's time actually went.
type a2uiHotSpot struct {
	name   string
	selfMS int
}

// a2uiHotSpots walks the span tree and returns the top spans by self time,
// largest first (ties break on name for determinism). Zero-self spans —
// pure containers whose children account for all their time — are skipped.
func a2uiHotSpots(spans []spanView, limit int) []a2uiHotSpot {
	var all []a2uiHotSpot
	var walk func([]spanView)
	walk = func(spans []spanView) {
		for _, sp := range spans {
			self := sp.DurationMS
			for _, ch := range sp.Children {
				self -= ch.DurationMS
			}
			if self > 0 {
				all = append(all, a2uiHotSpot{name: a2uiSpanName(sp), selfMS: self})
			}
			walk(sp.Children)
		}
	}
	walk(spans)
	sort.Slice(all, func(i, j int) bool {
		if all[i].selfMS != all[j].selfMS {
			return all[i].selfMS > all[j].selfMS
		}
		return all[i].name < all[j].name
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// a2uiSpanName picks the most human label a span carries.
func a2uiSpanName(sp spanView) string {
	switch {
	case sp.Name != "":
		return sp.Name
	case sp.Tool != "":
		return sp.Tool
	default:
		return sp.SpanID
	}
}

// a2uiRunWall returns the timeline extent the flame graph scales against:
// the run's recorded wall time, widened to cover any span that ends past it
// (an open run's stats can trail its freshest spans).
func a2uiRunWall(stats statsView, spans []spanView) int64 {
	wall := stats.WallTimeMS
	var walk func([]spanView)
	walk = func(spans []spanView) {
		for _, sp := range spans {
			if end := int64(sp.StartOffsetMS) + int64(sp.DurationMS); end > wall {
				wall = end
			}
			walk(sp.Children)
		}
	}
	walk(spans)
	return wall
}

// a2uiRunSurfaceID names the surface for a given run resource. Each read
// gets a fresh surface (the MCP transport is request/response; subscriptions
// layer updates on top via resources/updated).
func a2uiRunSurfaceID(runID string) string { return "cairn-run-" + runID }

// a2uiBundleSurfaceID names the surface for a given bundle resource.
func a2uiBundleSurfaceID(bundleID string) string { return "cairn-bundle-" + bundleID }

// --- trace view ---------------------------------------------------------------

// a2uiRunView renders a trajectory run as an A2UI component tree:
//
//	Card "header"  — title, model, status, started_at, prompt (truncated)
//	Card "stats"   — wall time, span count, tool calls, tokens; a stacked
//	                 time-by-category bar with a glyph legend; the top
//	                 self-time hot spots
//	Card "spans"   — a TUI flame graph: a timeline ruler, then one row per
//	                 span with the tree-indented label in a fixed column and
//	                 a bar in a fixed-width gutter, positioned by start
//	                 offset, scaled by duration, glyphed by category (the
//	                 same ranking the legend names); expandable per-span
//	                 output stays behind the existing JSON resource (lazy by
//	                 design)
//
// The tree is capped at a2uiMaxRunSpans components — a run that big should
// be paged over the existing run JSON resource, not squeezed into a single
// surface.
const a2uiMaxRunSpans = 200

func a2uiRunView(run *trajectory.Run, resp runResponse) a2uiEnvelope {
	surfaceID := a2uiRunSurfaceID(run.PublicID)
	components := []a2uiComponent{}
	headerChildren := []string{}

	title := run.Title
	if title == "" {
		title = "(untitled run)"
	}
	headerChildren = append(headerChildren, "hdr-title")
	components = append(components, a2uiText("hdr-title", title, "h2"))

	metaBits := []string{}
	if run.Model != "" {
		metaBits = append(metaBits, run.Model)
	}
	if run.Status != "" {
		metaBits = append(metaBits, "status: "+string(run.Status))
	}
	if !run.StartedAt.IsZero() {
		metaBits = append(metaBits, "started "+run.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if len(metaBits) > 0 {
		headerChildren = append(headerChildren, "hdr-meta")
		components = append(components, a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"))
	}

	if run.Prompt != "" {
		prompt := run.Prompt
		const maxPrompt = 240
		if len(prompt) > maxPrompt {
			prompt = prompt[:maxPrompt] + "…"
		}
		headerChildren = append(headerChildren, "hdr-prompt")
		components = append(components, a2uiText("hdr-prompt", prompt, "body"))
	}

	components = append(components,
		a2uiColumn("hdr-col", headerChildren),
		a2uiCard("hdr", "hdr-col"),
	)

	// Stats card — small caption row of derived facts, then the category
	// story: a stacked share bar, its glyph legend (the same glyphs the
	// flame graph bars use), and the top self-time hot spots.
	statsChildren := []string{"stats-row"}
	row := []string{}
	stats := resp.Stats
	row = append(row,
		"stats-wall", "stats-spans", "stats-tools", "stats-tokens",
	)
	components = append(components,
		a2uiText("stats-wall", "wall "+a2uiMS(stats.WallTimeMS), "caption"),
		a2uiText("stats-spans", fmt.Sprintf("%d spans", stats.SpanCount), "caption"),
		a2uiText("stats-tools", fmt.Sprintf("%d tools", stats.ToolCallCount), "caption"),
		a2uiText("stats-tokens", fmt.Sprintf("%d tokens", stats.TokenCount), "caption"),
		a2uiRow("stats-row", row),
	)

	ranked, glyphs := a2uiRankCategories(stats.TimeByCategoryMS)
	if len(ranked) > 0 {
		var total int64
		for _, c := range ranked {
			total += c.ms
		}
		if bar := a2uiDistBar(ranked, a2uiDistBarW); bar != "" {
			statsChildren = append(statsChildren, "stats-dist")
			components = append(components, a2uiText("stats-dist", bar, "body"))
		}
		parts := make([]string, 0, len(ranked))
		for _, c := range ranked {
			seg := string(c.glyph) + " " + c.name
			if total > 0 {
				seg += fmt.Sprintf(" %d%%", (c.ms*100+total/2)/total)
			}
			parts = append(parts, seg)
		}
		statsChildren = append(statsChildren, "stats-legend")
		components = append(components, a2uiText("stats-legend", strings.Join(parts, " · "), "caption"))
	}

	if hot := a2uiHotSpots(resp.Spans, a2uiMaxHotSpots); len(hot) > 0 {
		parts := make([]string, 0, len(hot))
		for _, h := range hot {
			parts = append(parts,
				fmt.Sprintf("%s %s", a2uiLabelSanitizer.Replace(h.name), a2uiMS(int64(h.selfMS))))
		}
		statsChildren = append(statsChildren, "stats-hot")
		components = append(components, a2uiText("stats-hot", "hot: "+strings.Join(parts, " · "), "caption"))
	}

	components = append(components,
		a2uiColumn("stats-col", statsChildren),
		a2uiCard("stats", "stats-col"),
	)

	// Spans card — the TUI flame graph. The web waterfall's information
	// (offset, duration, depth, category) in the terminal's native encoding:
	// a fixed label column carrying the tree indent, then a fixed-width
	// timeline gutter where each span's bar sits at its start offset, scaled
	// to its duration, drawn in its category's legend glyph. Rows live in a
	// Column, not a List — a List's per-item bullets would shift the gutter
	// out of column. A run with no measurable wall time renders plain
	// label · duration rows; blank gutters would just be noise.
	wall := a2uiRunWall(stats, resp.Spans)
	rowIDs := []string{}
	if wall > 0 && len(resp.Spans) > 0 {
		components = append(components,
			a2uiText("flame-axis",
				strings.Repeat(" ", a2uiFlameLabelW)+"▕"+a2uiFlameAxis(wall)+"▏", "caption"))
		rowIDs = append(rowIDs, "flame-axis")
	}
	spanCount := 0
	var walk func(spans []spanView)
	walk = func(spans []spanView) {
		for _, sp := range spans {
			if spanCount >= a2uiMaxRunSpans {
				return
			}
			spanCount++
			id := fmt.Sprintf("span-%d", spanCount)
			label := a2uiIndent(min(sp.Depth, a2uiFlameMaxDepth))
			if sp.Category != "" {
				label += "[" + sp.Category + "] "
			}
			label = a2uiLabelSanitizer.Replace(label + a2uiSpanName(sp))
			var line string
			if wall > 0 {
				glyph, ok := glyphs[sp.Category]
				if !ok {
					glyph = a2uiGlyphOther
				}
				line = a2uiPadLabel(label, a2uiFlameLabelW) +
					"▕" + a2uiFlameBar(sp.StartOffsetMS, sp.DurationMS, wall, glyph) + "▏ " +
					a2uiMS(int64(sp.DurationMS))
			} else {
				line = label + " · " + a2uiMS(int64(sp.DurationMS))
			}
			rowIDs = append(rowIDs, id)
			components = append(components, a2uiText(id, line, "body"))
			walk(sp.Children)
		}
	}
	walk(resp.Spans)
	if len(resp.Spans) > 0 && spanCount >= a2uiMaxRunSpans {
		rowIDs = append(rowIDs, "span-overflow")
		components = append(components,
			a2uiText("span-overflow",
				fmt.Sprintf("…%d more spans (read the JSON resource for the full tree)", stats.SpanCount-spanCount),
				"caption"))
	}

	if spanCount == 0 {
		rowIDs = append(rowIDs, "span-empty")
		components = append(components, a2uiText("span-empty", "no spans yet", "caption"))
	}

	components = append(components,
		a2uiColumn("spans-col", rowIDs),
		a2uiCard("spans", "spans-col"),
	)

	root := []string{"hdr", "stats", "spans"}
	components = append(components, a2uiColumn("root", root))

	return a2uiEnvelope{
		Version: a2uiVersion,
		UpdateComponents: a2uiUpdateComponents{
			SurfaceID:  surfaceID,
			CatalogID:  a2uiCatalog,
			Components: components,
		},
	}
}

// mcpReadRunA2UI is the ResourceHandler for the cairn://run/{id}/a2ui
// template: the same core read mcpReadRun performs, projected onto A2UI
// components instead of JSON. Reading requires artifacts:read, the same
// single scope as the JSON form.
func (s *Server) mcpReadRunA2UI(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read run/a2ui", oauth.ScopeArtifactsRead)
	}
	id, ok := matchRunA2UIURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a run a2ui resource URI", req.Params.URI)
	}
	run, err := s.traj.GetRun(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run/a2ui", err)
	}
	env := a2uiRunView(run, s.toRunResponse(run))
	body, err := json.Marshal(env)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run/a2ui", fmt.Errorf("encode a2ui run: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: a2uiMIME, Text: string(body)},
	}}, nil
}

// matchRunA2UIURI extracts the run id from a resolved cairn://run/<id>/a2ui
// URI. Both the bare mcp://cairn/run/<id>/a2ui form and the cairn:// alias
// are accepted (the registered template advertises the mcp:// form; the
// cairn:// form is what the issue spec calls out and what a hand-written
// @-mention would name).
func matchRunA2UIURI(uri string) (string, bool) {
	for _, prefix := range []string{"mcp://cairn/run/", "cairn://run/"} {
		if strings.HasPrefix(uri, prefix) {
			rest := strings.TrimPrefix(uri, prefix)
			id, found := strings.CutSuffix(rest, "/a2ui")
			if !found || id == "" || strings.Contains(id, "/") {
				return "", false
			}
			return id, true
		}
	}
	return "", false
}

// --- bundle view --------------------------------------------------------------

// a2uiMaxBundleMembers caps how many member rows render on one surface; a
// bundle past it should be paged over the JSON resource instead.
const a2uiMaxBundleMembers = 100

// a2uiSizeBarW is the rune width of a member's relative-size bar.
const a2uiSizeBarW = 8

// a2uiMemberView is the bundle-member projection the A2UI surface renders:
// the stored row plus the derived fields the web viewer's file rail shows —
// type badge and engagement counts — so the two surfaces say the same
// things about a member (ADR-0003 parity).
type a2uiMemberView struct {
	Name      string
	Size      int64
	MediaType string
	Badge     string
	Comments  int
	Reactions int
}

// a2uiSizeBar draws a member's size relative to the bundle's largest member
// as a fixed-width inline bar — the rail's at-a-glance answer to "which of
// these files is the big one". A non-empty member always draws at least one
// cell; the empty track renders as middots so the bar keeps its width.
func a2uiSizeBar(size, largest int64, width int) string {
	filled := 0
	if largest > 0 && size > 0 {
		filled = int(size * int64(width) / largest)
		if filled < 1 {
			filled = 1
		}
		if filled > width {
			filled = width
		}
	}
	return strings.Repeat("█", filled) + strings.Repeat("·", width-filled)
}

// a2uiBundleView renders a bundle as an envelope header Card (title, member
// count, total size, type mix, visibility, expiry) plus a member List whose
// rows mirror the web viewer's file rail: name, relative-size bar, size,
// type badge, media type, and comment/reaction counts when the member has
// engagement. Members render inside one List — the host bullets each row —
// rather than a bordered Card each, which buried a ten-file bundle in
// chrome.
func a2uiBundleView(bundleID string, art *artifact.Artifact, members []a2uiMemberView) a2uiEnvelope {
	surfaceID := a2uiBundleSurfaceID(bundleID)
	components := []a2uiComponent{}

	title := art.Title
	if title == "" {
		title = "(untitled bundle)"
	}
	headerChildren := []string{"hdr-title", "hdr-meta"}
	components = append(components,
		a2uiText("hdr-title", title, "h2"),
	)

	var totalSize, largest int64
	badgeCounts := map[string]int{}
	for _, m := range members {
		totalSize += m.Size
		if m.Size > largest {
			largest = m.Size
		}
		if m.Badge != "" {
			badgeCounts[m.Badge]++
		}
	}
	metaBits := []string{
		fmt.Sprintf("%d members", len(members)),
		a2uiHumanBytes(totalSize) + " total",
		"visibility: " + string(art.Access.Visibility),
		"expires " + art.ExpiresAt.UTC().Format("2006-01-02"),
	}
	components = append(components,
		a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"),
	)

	// Type mix — the rail's badge column aggregated ("3 MD · 2 CODE · 1
	// IMG"), most common first, ties alphabetical for determinism.
	if len(badgeCounts) > 0 {
		type badgeEntry struct {
			badge string
			n     int
		}
		mix := make([]badgeEntry, 0, len(badgeCounts))
		for b, n := range badgeCounts {
			mix = append(mix, badgeEntry{b, n})
		}
		sort.Slice(mix, func(i, j int) bool {
			if mix[i].n != mix[j].n {
				return mix[i].n > mix[j].n
			}
			return mix[i].badge < mix[j].badge
		})
		parts := make([]string, 0, len(mix))
		for _, e := range mix {
			parts = append(parts, fmt.Sprintf("%d %s", e.n, e.badge))
		}
		headerChildren = append(headerChildren, "hdr-mix")
		components = append(components, a2uiText("hdr-mix", strings.Join(parts, " · "), "caption"))
	}

	components = append(components,
		a2uiColumn("hdr-col", headerChildren),
		a2uiCard("hdr", "hdr-col"),
	)

	memberIDs := []string{}
	capped := members
	overflow := 0
	if len(capped) > a2uiMaxBundleMembers {
		overflow = len(capped) - a2uiMaxBundleMembers
		capped = capped[:a2uiMaxBundleMembers]
	}
	for i, m := range capped {
		colID := fmt.Sprintf("member-%d-col", i)
		nameID := fmt.Sprintf("member-%d-name", i)
		metaID := fmt.Sprintf("member-%d-meta", i)

		metaBits := []string{
			a2uiSizeBar(m.Size, largest, a2uiSizeBarW) + " " + a2uiHumanBytes(m.Size),
		}
		if m.Badge != "" {
			metaBits = append(metaBits, m.Badge)
		}
		if m.MediaType != "" {
			metaBits = append(metaBits, m.MediaType)
		}
		if m.Comments > 0 {
			metaBits = append(metaBits, fmt.Sprintf("💬 %d", m.Comments))
		}
		if m.Reactions > 0 {
			metaBits = append(metaBits, fmt.Sprintf("♥ %d", m.Reactions))
		}

		components = append(components,
			a2uiText(nameID, m.Name, "h5"),
			a2uiText(metaID, strings.Join(metaBits, " · "), "caption"),
			a2uiColumn(colID, []string{nameID, metaID}),
		)
		memberIDs = append(memberIDs, colID)
	}
	if overflow > 0 {
		memberIDs = append(memberIDs, "members-overflow")
		components = append(components,
			a2uiText("members-overflow",
				fmt.Sprintf("…%d more members (read the JSON resource for the full list)", overflow),
				"caption"))
	}

	root := []string{"hdr"}
	if len(memberIDs) > 0 {
		components = append(components,
			a2uiList("members-list", memberIDs),
			a2uiCard("members", "members-list"),
		)
		root = append(root, "members")
	} else {
		components = append(components,
			a2uiText("members-empty", "no members in this bundle", "caption"),
		)
		root = append(root, "members-empty")
	}
	components = append(components, a2uiColumn("root", root))

	return a2uiEnvelope{
		Version: a2uiVersion,
		UpdateComponents: a2uiUpdateComponents{
			SurfaceID:  surfaceID,
			CatalogID:  a2uiCatalog,
			Components: components,
		},
	}
}

// mcpReadBundleA2UI is the ResourceHandler for cairn://bundle/{id}/a2ui:
// the same core read the artifact/bundle JSON handlers perform (a
// bundle-shaped artifact plus its member list), projected onto A2UI
// components — enriched with the derived fields the web viewer's file rail
// computes: per-member type badge (registry classification, ADR-0002) and
// engagement counts (comments + reactions anchored to the member,
// ADR-0006). Annotation reads are best-effort: a listing failure costs the
// counts, never the surface. Reading requires artifacts:read.
func (s *Server) mcpReadBundleA2UI(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read bundle/a2ui", oauth.ScopeArtifactsRead)
	}
	id, ok := matchBundleA2UIURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a bundle a2ui resource URI", req.Params.URI)
	}
	art, err := s.store.GetByPublicID(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read bundle/a2ui", err)
	}
	if art.ShareType != artifact.TypeBundle {
		return nil, s.mcpToolErr(ctx, "resources/read bundle/a2ui",
			errs.Validationf("%s is a %s, not a bundle", id, art.ShareType))
	}
	memberRows, err := s.store.ListMembers(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read bundle/a2ui", err)
	}
	commentCounts := map[string]int{}
	if s.annot != nil {
		if comments, err := s.annot.ListComments(ctx, id); err == nil {
			commentCounts = memberCommentCounts(comments)
		}
	}
	reactionCounts := s.memberReactionCounts(ctx, id)
	members := make([]a2uiMemberView, 0, len(memberRows))
	for _, m := range memberRows {
		badge := ""
		if s.reg != nil {
			badge = s.reg.Resolve(s.reg.ClassifyMember(m.Name, m.MediaType)).Badge()
		}
		members = append(members, a2uiMemberView{
			Name:      m.Name,
			Size:      m.Size,
			MediaType: m.MediaType,
			Badge:     badge,
			Comments:  commentCounts[m.Name],
			Reactions: reactionCounts[m.Name],
		})
	}
	env := a2uiBundleView(id, art, members)
	body, err := json.Marshal(env)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read bundle/a2ui", fmt.Errorf("encode a2ui bundle: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: a2uiMIME, Text: string(body)},
	}}, nil
}

// matchBundleA2UIURI extracts the bundle id from a resolved
// cairn://bundle/<id>/a2ui URI.
func matchBundleA2UIURI(uri string) (string, bool) {
	for _, prefix := range []string{"mcp://cairn/bundle/", "cairn://bundle/"} {
		if strings.HasPrefix(uri, prefix) {
			rest := strings.TrimPrefix(uri, prefix)
			id, found := strings.CutSuffix(rest, "/a2ui")
			if !found || id == "" || strings.Contains(id, "/") {
				return "", false
			}
			return id, true
		}
	}
	return "", false
}

// --- single-body artifact view ------------------------------------------------

// a2uiMaxBodyBytes caps how much of the artifact body is rendered as A2UI text.
// A host that wants the full body reads the JSON/REST resource; the A2UI surface
// is a readable card, not a dump.
const a2uiMaxBodyBytes = 8192

// a2uiArtifactSurfaceID names the surface for a given artifact resource.
func a2uiArtifactSurfaceID(publicID string) string { return "cairn-art-" + publicID }

// a2uiArtifactView renders a single-body artifact as an A2UI component tree:
// a header Card (title, media type, size, visibility, expiry) and a body Card
// containing the text content.
func a2uiArtifactView(art *artifact.Artifact, body string) a2uiEnvelope {
	surfaceID := a2uiArtifactSurfaceID(art.PublicID)
	components := []a2uiComponent{}

	title := art.Title
	if title == "" {
		title = "(untitled artifact)"
	}

	headerChildren := []string{"hdr-title", "hdr-meta"}
	components = append(components,
		a2uiText("hdr-title", title, "h2"),
	)

	metaBits := []string{
		a2uiHumanBytes(art.Size),
		art.MediaType,
		"visibility: " + string(art.Access.Visibility),
		"expires " + art.ExpiresAt.UTC().Format("2006-01-02"),
	}
	components = append(components,
		a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"),
		a2uiColumn("hdr-col", headerChildren),
		a2uiCard("hdr", "hdr-col"),
	)

	bodyText := body
	if len(bodyText) > a2uiMaxBodyBytes {
		bodyText = bodyText[:a2uiMaxBodyBytes] + "\n\n…(truncated — read the full artifact via artifact_read)"
	}

	components = append(components,
		a2uiText("body-text", bodyText, "body"),
		a2uiColumn("body-col", []string{"body-text"}),
		a2uiCard("body", "body-col"),
	)

	root := []string{"hdr", "body"}
	components = append(components, a2uiColumn("root", root))

	return a2uiEnvelope{
		Version: a2uiVersion,
		UpdateComponents: a2uiUpdateComponents{
			SurfaceID:  surfaceID,
			CatalogID:  a2uiCatalog,
			Components: components,
		},
	}
}

// artifactIsBodyless reports whether the share type carries no single
// content-addressed body. Mirrors artifact.Artifact.bodyless() but is callable
// from this package (bodyless is unexported).
func artifactIsBodyless(t artifact.ShareType) bool {
	return t == artifact.TypeBundle || t == artifact.TypeTrajectory || t == artifact.TypeWebhook
}

// mcpReadArtifactA2UI is the ResourceHandler for cairn://artifact/{id}/a2ui:
// reads a single-body artifact and projects it onto A2UI components. Rejects
// bodyless types (bundle, trajectory, webhook) — those have their own surfaces.
// Reading requires artifacts:read.
func (s *Server) mcpReadArtifactA2UI(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read artifact/a2ui", oauth.ScopeArtifactsRead)
	}
	id, ok := matchArtifactA2UIURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not an artifact a2ui resource URI", req.Params.URI)
	}
	art, err := s.store.GetByPublicID(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui", err)
	}
	if artifactIsBodyless(art.ShareType) {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui",
			errs.Validationf("%s is a %s, not a single-body artifact; use the %s a2ui surface instead",
				id, art.ShareType, art.ShareType))
	}

	rc, _, err := s.store.OpenBody(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, a2uiMaxBodyBytes+1))
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui", fmt.Errorf("read body: %w", err))
	}

	env := a2uiArtifactView(art, string(body))
	out, err := json.Marshal(env)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui", fmt.Errorf("encode a2ui artifact: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: a2uiMIME, Text: string(out)},
	}}, nil
}

// matchArtifactA2UIURI extracts the artifact id from a resolved
// cairn://artifact/<id>/a2ui URI.
func matchArtifactA2UIURI(uri string) (string, bool) {
	for _, prefix := range []string{"mcp://cairn/artifact/", "cairn://artifact/"} {
		if strings.HasPrefix(uri, prefix) {
			rest := strings.TrimPrefix(uri, prefix)
			id, found := strings.CutSuffix(rest, "/a2ui")
			if !found || id == "" || strings.Contains(id, "/") {
				return "", false
			}
			return id, true
		}
	}
	return "", false
}
