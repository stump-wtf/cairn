// A2UI (Agent-to-UI) resources: render `application/a2ui+json` projections of
// a trajectory run's waterfall + stream, and a bundle's member cards, so an
// A2UI-capable MCP host (Crush once joestump-agent/crush#217 lands) draws the
// view inline instead of asking the model to re-render raw JSON.
//
// Three resources ship, all read-only at the A2UI layer (mutations stay on
// the existing MCP tools; the surfaces are text-only — no buttons or actions
// are emitted — until the `a2ui_action` round-trip lands in
// joestump-agent/crush#221):
//
//	cairn://run/{id}/a2ui       — trace header + stats (category bar, hot
//	                              spots) + a span flame graph: per-span
//	                              timeline bars in a fixed-width gutter
//	cairn://bundle/{id}/a2ui    — bundle envelope (totals, type mix) + a
//	                              member list with badges, relative size
//	                              bars and engagement counts
//	cairn://artifact/{id}/a2ui  — single-body artifact (markdown, code, file)
//
// All follow the A2UI-over-MCP transport contract:
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.stump.rocks/stump.wtf/md2a2ui"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/oauth"
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

// a2uiButton wraps a single child in a Button carrying an event action. The
// v0.9 standard catalog's ButtonComponent requires `action` and `child`
// (`variant` optional, omitted here). The action's `context` is the semantic
// payload the host resolves against surface state and sends back to the
// server's a2ui_action tool — ids and names travel raw (never
// percent-escaped); any URI encoding is the host's concern when it
// re-requests a resource. Read-only navigation only: no mutation verbs.
func a2uiButton(id, childID, actionName string, ctx map[string]any) a2uiComponent {
	return a2uiComponent{
		"id":        id,
		"component": "Button",
		"action": map[string]any{
			"event": map[string]any{
				"name":    actionName,
				"context": ctx,
			},
		},
		"child": childID,
	}
}

// a2uiHeaderCard builds the header Card the bundle and artifact surfaces lead
// with: an h2 title plus a caption row of metadata bits, wrapped in a Column
// inside a Card under the shared hdr-* component ids. (The run surface builds
// its header separately — its meta row and prompt are both optional.)
func a2uiHeaderCard(title string, metaBits []string) []a2uiComponent {
	return []a2uiComponent{
		a2uiText("hdr-title", title, "h2"),
		a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"),
		a2uiColumn("hdr-col", []string{"hdr-title", "hdr-meta"}),
		a2uiCard("hdr", "hdr-col"),
	}
}

// matchA2UIURI extracts the id from a resolved <scheme>://<kind>/<id>/a2ui
// URI, where kind is "run", "bundle" or "artifact". Both the bare
// mcp://cairn/<kind>/<id>/a2ui form and the cairn://<kind>/<id>/a2ui alias
// are accepted (the registered templates advertise the mcp:// form; the
// cairn:// form is what the issue spec calls out and what a hand-written
// @-mention would name).
func matchA2UIURI(uri, kind string) (string, bool) {
	for _, prefix := range []string{"mcp://cairn/" + kind + "/", "cairn://" + kind + "/"} {
		if strings.HasPrefix(uri, prefix) {
			rest := strings.TrimPrefix(uri, prefix)
			// A query string (e.g. the run template's optional ?w= width
			// hint) must come off before the /a2ui suffix check.
			path, _, _ := strings.Cut(rest, "?")
			id, found := strings.CutSuffix(path, "/a2ui")
			if !found || id == "" || strings.Contains(id, "/") {
				return "", false
			}
			return id, true
		}
	}
	return "", false
}

// --- bundle-member navigation (#103) ------------------------------------------

// a2uiMemberURIEscape percent-encodes a member name for the single {name}
// path segment of a cairn://bundle/{id}/{name}/a2ui URI. The SDK's URI-template
// {name} variable matches a single segment only — a literal "/" in the URI
// fails to match — so a nested member name (dir/file.md) must travel with "/"
// escaped to %2F. url.PathEscape leaves "/" alone, so we escape then fold the
// remaining separators. Spaces become %20 (not '+': this is a path segment,
// not a query). This is the inverse of the decode in matchA2UIMemberURI.
func a2uiMemberURIEscape(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), "/", "%2F")
}

// matchA2UIMemberURI extracts the bundle id and member name from a resolved
// <scheme>://bundle/<id>/<name>/a2ui URI. Both the mcp:// and cairn:// forms
// are accepted. The member name is percent-decoded after capture (the SDK
// matches the escaped form; a nested name arrives as %2F). A query string
// (?w= width hint) is stripped first, as in matchA2UIURI. Returns ok=false for
// any shape that is not exactly bundle/<id>/<single name segment>/a2ui — a URI
// whose name segment still contains a literal "/" after decoding is rejected
// (that shape never matched the registered template anyway).
func matchA2UIMemberURI(uri string) (id, name string, ok bool) {
	for _, prefix := range []string{"mcp://cairn/bundle/", "cairn://bundle/"} {
		if !strings.HasPrefix(uri, prefix) {
			continue
		}
		rest := strings.TrimPrefix(uri, prefix)
		path, _, _ := strings.Cut(rest, "?")
		body, found := strings.CutSuffix(path, "/a2ui")
		if !found {
			return "", "", false
		}
		// body is "<id>/<name>" with exactly one "/" separating them — the
		// {name} template var is single-segment, so the raw URI cannot carry
		// a second "/".
		idPart, namePart, found := strings.Cut(body, "/")
		if !found || idPart == "" || namePart == "" || strings.Contains(namePart, "/") {
			return "", "", false
		}
		decoded, err := url.PathUnescape(namePart)
		if err != nil || decoded == "" {
			return "", "", false
		}
		return idPart, decoded, true
	}
	return "", "", false
}

// a2uiMemberIDSanitize makes a member name safe for a component/surface id:
// any run of non-alphanumeric bytes becomes a single dash. Member names carry
// "/", ".", " " and other separators that are illegal in component ids.
func a2uiMemberIDSanitize(name string) string {
	var b strings.Builder
	lastDash := true // collapse leading punctuation into no leading dash
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// a2uiMemberSurfaceID names the surface for one bundle member. Distinct from
// the bundle surface (a2uiBundleSurfaceID) so a host's navigation stack can
// tell "the bundle list" from "the open member" apart.
func a2uiMemberSurfaceID(bundleID, memberName string) string {
	return "cairn-bundle-" + bundleID + "-member-" + a2uiMemberIDSanitize(memberName)
}

// a2uiRequestedWidth reads the run template's optional ?w=N width hint: the
// total flame-row budget the host wants, converted to the gutter width the
// renderer scales bars against. The TOTAL is clamped to [a2uiWidthMin,
// a2uiWidthMax] before the fixed overhead comes off — clamping the gutter
// instead would hand a narrow host rows wider than it asked for, which is
// the exact wrap ?w= exists to prevent. Absent or malformed values fall
// back to the default budget.
func a2uiRequestedWidth(uri string) int {
	_, query, hasQuery := strings.Cut(uri, "?")
	if !hasQuery {
		return a2uiFlameGutterW
	}
	w, err := strconv.Atoi(strings.TrimPrefix(query, "w="))
	if err != nil {
		return a2uiFlameGutterW
	}
	return min(max(w, a2uiWidthMin), a2uiWidthMax) - a2uiWidthOverhead
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

// a2uiTruncate cuts s at max bytes, walking the cut back so it never splits a
// multi-byte UTF-8 rune (json.Marshal coerces a torn sequence to U+FFFD, which
// would end the user-facing card in a replacement glyph). Returns s unchanged
// when it already fits.
func a2uiTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
// host's terminal width, so rows are built to a fixed budget. The default
// (~96 cells: label 26 + gutter edges 2 + gutter 60 + duration ~8) fills a
// wide host's card interior; width-aware hosts can request an exact budget
// with the ?w= query parameter (clamped to [60, 200] — the ?w value is the
// TOTAL row width, so a host asking for its own content width never gets
// rows wider than it asked for).
const (
	a2uiFlameLabelW   = 26 // rune width of the span-label column
	a2uiFlameGutterW  = 60 // rune width of the timeline gutter between ▕ ▏
	a2uiFlameMaxDepth = 6  // indent cap so deep trees don't consume the label
	a2uiDistBarW      = 48 // width of the stats time-by-category bar
	a2uiMaxHotSpots   = 3  // slowest-self-time spans named in the stats card

	a2uiWidthMin      = 60                      // smallest total width a ?w= request can set
	a2uiWidthMax      = 200                     // largest total width a ?w= request can set
	a2uiWidthOverhead = a2uiFlameLabelW + 2 + 8 // label + gutter edges + duration column
)

// a2uiCategoryPalette maps time-share rank onto a bar glyph, densest first,
// so the category that dominates the run draws the boldest bars. Categories
// are an OPEN set (trajectory.Category), so ranking — not a fixed name→glyph
// table — is what keeps any vocabulary renderable. Its length is also the
// legend cap: categories past it fold into one "other" bucket.
var a2uiCategoryPalette = []rune{'█', '▓', '▒', '░', '▞'}

// a2uiCategoryColors maps the same rank onto an ANSI 256-color code, so the
// glyph a category draws in the dist bar, the legend, and every flame row
// shares one hue. The v0.9 Text component has no color property, but ANSI
// sequences survive the wire (JSON), the validator (plain strings), and the
// host renderers (lipgloss measures cells, not bytes) — so color is emitted
// inline. Hosts that strip ANSI still get the glyph encoding; nothing else
// changes.
var a2uiCategoryColors = []int{42, 45, 178, 111, 203}

// a2uiColorOther is the hue for categories folded past the palette.
const a2uiColorOther = 244

// a2uiGlyphOther marks categories folded past the palette.
const a2uiGlyphOther = '▪'

// a2uiANSIOn flips ANSI color emission. Tests disable it so geometry
// assertions stay byte-exact.
var a2uiANSIOn = true

// a2uiColorize wraps s in an ANSI 256-color foreground (and reset) when
// color emission is enabled.
func a2uiColorize(code int, s string) string {
	if !a2uiANSIOn {
		return s
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", code, s)
}

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
// its assigned bar glyph and ANSI color.
type a2uiCatShare struct {
	name  string
	ms    int64
	glyph rune
	color int
}

// a2uiRankCategories orders time-by-category descending (ties break on name
// so the output is deterministic), assigns palette glyphs and colors to the
// top entries, and folds the remainder into one synthetic "other" entry. The
// returned map looks up the bar glyph and color for any category the run
// used — including the folded ones, which all draw a2uiGlyphOther.
func a2uiRankCategories(byCat map[string]int64) ([]a2uiCatShare, map[string]a2uiCatShare) {
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
	lookup := make(map[string]a2uiCatShare, len(ranked))
	if len(ranked) > len(a2uiCategoryPalette) {
		var otherMS int64
		for _, c := range ranked[len(a2uiCategoryPalette):] {
			otherMS += c.ms
			lookup[c.name] = a2uiCatShare{glyph: a2uiGlyphOther, color: a2uiColorOther}
		}
		ranked = append(ranked[:len(a2uiCategoryPalette)],
			a2uiCatShare{name: "other", ms: otherMS, glyph: a2uiGlyphOther, color: a2uiColorOther})
	}
	for i := range ranked {
		if ranked[i].glyph == 0 {
			ranked[i].glyph = a2uiCategoryPalette[i]
			ranked[i].color = a2uiCategoryColors[i%len(a2uiCategoryColors)]
		}
		lookup[ranked[i].name] = ranked[i]
	}
	return ranked, lookup
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
// [start, start+duration) range scaled onto gutterW cells, in the span
// category's ANSI color. Every span draws at least one cell — a 3ms tool
// call is a real event, not invisible — and the bar is clamped inside the
// gutter whatever the offsets claim.
func a2uiFlameBar(startMS, durMS int, wallMS int64, gutterW int, cat a2uiCatShare) string {
	glyph := cat.glyph
	if glyph == 0 {
		glyph = a2uiGlyphOther
	}
	gw := int64(gutterW)
	start := 0
	if wallMS > 0 {
		start = int(int64(startMS) * gw / wallMS)
	}
	if start < 0 {
		start = 0
	}
	if start > gutterW-1 {
		start = gutterW - 1
	}
	width := int(int64(durMS) * gw / max(wallMS, 1))
	if width < 1 {
		width = 1
	}
	if start+width > gutterW {
		width = gutterW - start
	}
	return strings.Repeat(" ", start) +
		a2uiColorize(cat.color, strings.Repeat(string(glyph), width)) +
		strings.Repeat(" ", gutterW-start-width)
}

// a2uiFlameAxis draws the timeline ruler the bars hang under: zero at the
// left edge, the run's wall time at the right.
func a2uiFlameAxis(wallMS int64, gutterW int) string {
	right := a2uiMS(wallMS)
	fill := gutterW - 1 - len([]rune(right))
	if fill < 1 {
		fill = 1
	}
	return "0" + strings.Repeat("┄", fill) + right
}

// a2uiDistBar renders the ranked category shares as one fixed-width stacked
// bar, each segment in its category's ANSI color. Cell counts come from
// cumulative rounding so they always sum to the bar width exactly; a
// category too small for a cell of its own disappears into its neighbour
// rather than inflating the bar.
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
			b.WriteString(a2uiColorize(c.color, strings.Repeat(string(c.glyph), n)))
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

// a2uiRunSurfaceID names the surface for a given run resource. Each read gets
// a fresh surface — the MCP transport is request/response, and the a2ui
// surfaces themselves are not subscribable (mcpSubscribeResource rejects
// them). A host that wants live updates subscribes to the JSON run resource
// and re-reads this surface on each resources/updated notification.
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
// The surface is text-only — no buttons or actions are emitted — until the
// a2ui_action round-trip lands (joestump-agent/crush#221). The tree is
// capped at a2uiMaxRunSpans components — a run that big should be paged over
// the existing run JSON resource, not squeezed into a single surface.
const a2uiMaxRunSpans = 200

func a2uiRunView(resp runResponse, gutterW int) a2uiEnvelope {
	surfaceID := a2uiRunSurfaceID(resp.ID)
	components := []a2uiComponent{}
	headerChildren := []string{}

	title := resp.Title
	if title == "" {
		title = "(untitled run)"
	}
	headerChildren = append(headerChildren, "hdr-title")
	components = append(components, a2uiText("hdr-title", title, "h2"))

	metaBits := []string{}
	if resp.Model != "" {
		metaBits = append(metaBits, resp.Model)
	}
	if resp.Status != "" {
		metaBits = append(metaBits, "status: "+resp.Status)
	}
	if !resp.StartedAt.IsZero() {
		metaBits = append(metaBits, "started "+resp.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if len(metaBits) > 0 {
		headerChildren = append(headerChildren, "hdr-meta")
		components = append(components, a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"))
	}

	if resp.Prompt != "" {
		prompt := resp.Prompt
		const maxPrompt = 240
		if len(prompt) > maxPrompt {
			prompt = a2uiTruncate(prompt, maxPrompt) + "…"
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

	ranked, lookup := a2uiRankCategories(stats.TimeByCategoryMS)
	if len(ranked) > 0 {
		var total int64
		for _, c := range ranked {
			total += c.ms
		}
		// The dist bar never renders wider than the flame gutter, so a
		// narrow ?w= host doesn't get a stats row that wraps while its
		// flame rows fit.
		if bar := a2uiDistBar(ranked, min(a2uiDistBarW, gutterW)); bar != "" {
			statsChildren = append(statsChildren, "stats-dist")
			components = append(components, a2uiText("stats-dist", bar, "body"))
		}
		parts := make([]string, 0, len(ranked))
		for _, c := range ranked {
			seg := a2uiColorize(c.color, string(c.glyph)) + " " + c.name
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
				strings.Repeat(" ", a2uiFlameLabelW)+"▕"+a2uiFlameAxis(wall, gutterW)+"▏", "caption"))
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
				cat, ok := lookup[sp.Category]
				if !ok {
					cat = a2uiCatShare{glyph: a2uiGlyphOther, color: a2uiColorOther}
				}
				line = a2uiPadLabel(label, a2uiFlameLabelW) +
					"▕" + a2uiFlameBar(sp.StartOffsetMS, sp.DurationMS, wall, gutterW, cat) + "▏ " +
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
	// Only when spans were actually omitted: a tree holding exactly the cap
	// renders whole, and a "…0 more spans" row would falsely claim data is
	// missing.
	if stats.SpanCount > spanCount {
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
	id, ok := matchA2UIURI(req.Params.URI, "run")
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a run a2ui resource URI", req.Params.URI)
	}
	run, err := s.traj.GetRun(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run/a2ui", err)
	}
	env := a2uiRunView(s.toRunResponse(run), a2uiRequestedWidth(req.Params.URI))
	body, err := json.Marshal(env)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read run/a2ui", fmt.Errorf("encode a2ui run: %w", err))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: a2uiMIME, Text: string(body)},
	}}, nil
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
// chrome. Like the trace view, the surface is text-only — no actions on the
// cards — until the a2ui_action round-trip lands (joestump-agent/crush#221).
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
		btnID := fmt.Sprintf("member-%d-btn", i)
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

		// Each member row is a Button wrapping the same name+meta content, so
		// the list looks identical but the row is clickable on an
		// A2UI-action-capable host (joestump-agent/crush#221). The open_member
		// action's context carries {bundle, member} raw so the server's
		// a2ui_action round-trip (#106) is self-contained — no host-side
		// surfaceId parsing needed.
		components = append(components,
			a2uiText(nameID, m.Name, "h5"),
			a2uiText(metaID, strings.Join(metaBits, " · "), "caption"),
			a2uiColumn(colID, []string{nameID, metaID}),
			a2uiButton(btnID, colID, "open_member", map[string]any{
				"bundle": bundleID,
				"member": m.Name,
			}),
		)
		memberIDs = append(memberIDs, btnID)
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
	id, ok := matchA2UIURI(req.Params.URI, "bundle")
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

	components = append(components, a2uiHeaderCard(title, []string{
		a2uiHumanBytes(art.Size),
		art.MediaType,
		"visibility: " + string(art.Access.Visibility),
		"expires " + art.ExpiresAt.UTC().Format("2006-01-02"),
	})...)

	// For markdown artifacts, convert the body to structured A2UI components
	// (headings, lists, tables, code blocks, blockquotes) via md2a2ui.
	// Falls through to plain text if the conversion fails or the media type
	// is not markdown.
	if isMarkdownMediaType(art.MediaType) {
		msg, err := md2a2ui.ConvertWithSurface(body, surfaceID+"-md")
		if err == nil && msg != nil && msg.UpdateComponents != nil && len(msg.UpdateComponents.Components) > 0 {
			for _, mc := range msg.UpdateComponents.Components {
				mc.ID = "md-" + mc.ID
				for i := range mc.Children {
					mc.Children[i] = "md-" + mc.Children[i]
				}
				if mc.Child != "" {
					mc.Child = "md-" + mc.Child
				}
				components = append(components, convertMDComponent(mc))
			}
			// The root component from md2a2ui is a Column named "md-root";
			// its children become the body card's children.
			components = append(components,
				a2uiCard("body", "md-root"),
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
	}

	bodyText := body
	if len(bodyText) > a2uiMaxBodyBytes {
		bodyText = a2uiTruncate(bodyText, a2uiMaxBodyBytes) + "\n\n…(truncated — read the full artifact via artifact_read)"
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

// isMarkdownMediaType reports whether the media type is markdown and should
// be rendered through md2a2ui.
func isMarkdownMediaType(mt string) bool {
	return mt == "text/markdown" || mt == "text/x-markdown" || strings.HasSuffix(mt, "+markdown")
}

// convertMDComponent converts an md2a2ui.Component to an a2uiComponent map.
func convertMDComponent(c md2a2ui.Component) a2uiComponent {
	m := a2uiComponent{
		"id":        c.ID,
		"component": c.Component,
	}
	if c.Text != "" {
		m["text"] = c.Text
	}
	if c.Variant != "" {
		m["variant"] = c.Variant
	}
	if len(c.Children) > 0 {
		m["children"] = c.Children
	}
	if c.Child != "" {
		m["child"] = c.Child
	}
	if c.Axis != "" {
		m["axis"] = c.Axis
	}
	if c.URL != "" {
		m["url"] = c.URL
	}
	if c.AltText != "" {
		m["altText"] = c.AltText
	}
	if c.Direction != "" {
		m["direction"] = c.Direction
	}
	return m
}

// a2uiBodylessHint names the surface that actually renders a bodyless share
// type. The a2ui surfaces are keyed by resource kind, not share type — a
// trajectory renders at cairn://run/{id}/a2ui, and a webhook stream has no
// a2ui surface at all — so interpolating the share type into a "use the X
// a2ui surface" hint would send the caller to a URI no matcher accepts.
func a2uiBodylessHint(id string, t artifact.ShareType) string {
	switch t {
	case artifact.TypeTrajectory:
		return fmt.Sprintf("use cairn://run/%s/a2ui instead", id)
	case artifact.TypeBundle:
		return fmt.Sprintf("use cairn://bundle/%s/a2ui instead", id)
	default: // webhook
		return fmt.Sprintf("webhook streams have no a2ui surface; read mcp://cairn/hook/%s instead", id)
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
	id, ok := matchA2UIURI(req.Params.URI, "artifact")
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not an artifact a2ui resource URI", req.Params.URI)
	}
	art, err := s.store.GetByPublicID(ctx, id)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui", err)
	}
	if artifactIsBodyless(art.ShareType) {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui",
			errs.Validationf("%s is a %s, not a single-body artifact; %s",
				id, art.ShareType, a2uiBodylessHint(id, art.ShareType)))
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

	// A binary body (image, gz — creatable via the web upload path) has no
	// text to render: refuse it, as the registered template description
	// promises, instead of marshalling mojibake into a user-facing Text
	// component. The check mirrors artifact_read's utf8.Valid gate; that tool
	// falls back to base64, but an A2UI Text card has no binary encoding, so
	// this surface rejects. Validity is judged on the rune-safe prefix that
	// would actually render — the read is capped one byte past the render
	// limit, so a perfectly valid text body can end mid-rune at the cut.
	if !utf8.ValidString(a2uiTruncate(string(body), a2uiMaxBodyBytes)) {
		return nil, s.mcpToolErr(ctx, "resources/read artifact/a2ui",
			errs.Validationf("%s has a binary body (%s) with no a2ui text rendering; read it via artifact_read or the artifact URL",
				id, art.MediaType))
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

// --- bundle-member navigation view (#103, story #104) --------------------------

// renderMemberA2UI resolves one bundle member and marshals its A2UI envelope.
// Shared by the member resource handler (mcpReadMemberA2UI) and the a2ui_action
// round-trip (#106) so both return byte-identical components for the same
// member. The projection mirrors mcpReadArtifactA2UI: a header Card (member
// name, media type, size, type badge) plus the body — markdown via md2a2ui,
// other text in a body Card, binary rejected. Errors are already wrapped for
// the caller's mcpToolErr (not-found is the store's uniform one).
func (s *Server) renderMemberA2UI(ctx context.Context, bundleID, memberName string) ([]byte, error) {
	rc, info, err := s.store.OpenMember(ctx, bundleID, memberName)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, a2uiMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read member body: %w", err)
	}

	// Binary members have no A2UI text rendering, matching the artifact
	// surface's binary rejection (utf8 judged on the rune-safe render prefix).
	if !utf8.ValidString(a2uiTruncate(string(body), a2uiMaxBodyBytes)) {
		return nil, errs.Validationf("member %s/%s has a binary body (%s) with no a2ui text rendering; read it via artifact_read or the artifact URL",
			bundleID, memberName, info.MediaType)
	}

	badge := ""
	if s.reg != nil {
		badge = s.reg.Resolve(s.reg.ClassifyMember(memberName, info.MediaType)).Badge()
	}

	surfaceID := a2uiMemberSurfaceID(bundleID, memberName)
	metaBits := []string{a2uiHumanBytes(info.Size)}
	if badge != "" {
		metaBits = append(metaBits, badge)
	}
	if info.MediaType != "" {
		metaBits = append(metaBits, info.MediaType)
	}

	components := []a2uiComponent{}
	components = append(components, a2uiHeaderCard(memberName, metaBits)...)

	if isMarkdownMediaType(info.MediaType) {
		if msg, err := md2a2ui.ConvertWithSurface(string(body), surfaceID+"-md"); err == nil &&
			msg != nil && msg.UpdateComponents != nil && len(msg.UpdateComponents.Components) > 0 {
			for _, mc := range msg.UpdateComponents.Components {
				mc.ID = "md-" + mc.ID
				for i := range mc.Children {
					mc.Children[i] = "md-" + mc.Children[i]
				}
				if mc.Child != "" {
					mc.Child = "md-" + mc.Child
				}
				components = append(components, convertMDComponent(mc))
			}
			components = append(components,
				a2uiCard("body", "md-root"),
				a2uiColumn("root", []string{"hdr", "body"}),
			)
			return json.Marshal(a2uiEnvelope{
				Version: a2uiVersion,
				UpdateComponents: a2uiUpdateComponents{
					SurfaceID:  surfaceID,
					CatalogID:  a2uiCatalog,
					Components: components,
				},
			})
		}
	}

	bodyText := string(body)
	if len(bodyText) > a2uiMaxBodyBytes {
		bodyText = a2uiTruncate(bodyText, a2uiMaxBodyBytes) + "\n\n…(truncated — read the full member via artifact_read)"
	}
	components = append(components,
		a2uiText("body-text", bodyText, "body"),
		a2uiColumn("body-col", []string{"body-text"}),
		a2uiCard("body", "body-col"),
		a2uiColumn("root", []string{"hdr", "body"}),
	)
	return json.Marshal(a2uiEnvelope{
		Version: a2uiVersion,
		UpdateComponents: a2uiUpdateComponents{
			SurfaceID:  surfaceID,
			CatalogID:  a2uiCatalog,
			Components: components,
		},
	})
}

// mcpReadMemberA2UI is the ResourceHandler for cairn://bundle/{id}/{name}/a2ui:
// one bundle member rendered as its own A2UI surface — the navigation target a
// host lands on when the user opens a member from the bundle list. Reading
// requires artifacts:read.
func (s *Server) mcpReadMemberA2UI(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, s.mcpScopeErr(ctx, "resources/read bundle/member/a2ui", oauth.ScopeArtifactsRead)
	}
	id, name, ok := matchA2UIMemberURI(req.Params.URI)
	if !ok {
		return nil, fmt.Errorf("validation_failed: %q is not a bundle member a2ui resource URI", req.Params.URI)
	}
	out, err := s.renderMemberA2UI(ctx, id, name)
	if err != nil {
		return nil, s.mcpToolErr(ctx, "resources/read bundle/member/a2ui", err)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: req.Params.URI, MIMEType: a2uiMIME, Text: string(out)},
	}}, nil
}

// --- action round-trip (#103, story #106) -------------------------------------

// mcpA2UIActionInput is the a2ui_action tool's input per the A2UI-over-MCP
// contract (https://a2ui.org/guides/a2ui_over_mcp/): the host resolves data
// bindings in Context against surface state and calls with the action name.
type mcpA2UIActionInput struct {
	// Name is the action's event name (e.g. "open_member").
	Name string `json:"name"`
	// Context carries the action's ids/names raw (never percent-escaped).
	Context map[string]any `json:"context,omitempty"`
}

// a2uiActionOpenMember is the single navigation verb the bundle surface emits.
const a2uiActionOpenMember = "open_member"

// mcpA2UIAction is the a2ui_action tool handler: the server side of the
// A2UI-over-MCP action round-trip. It resolves the action and returns the
// result as an EmbeddedResource (MIME application/a2ui+json) so the host feeds
// it back into the SAME surface in place (no agent turn), plus a TextContent
// fallback for non-A2UI callers (spec best practice). Today the only verb is
// open_member → the member surface; the projection is shared with the member
// resource (#104) so the round-trip and a direct read return identical
// components. Reading requires artifacts:read.
func (s *Server) mcpA2UIAction(ctx context.Context, req *mcp.CallToolRequest, in mcpA2UIActionInput) (*mcp.CallToolResult, any, error) {
	if !mcpScopes(req.Extra)[oauth.ScopeArtifactsRead] {
		return nil, nil, s.mcpScopeErr(ctx, "a2ui_action", oauth.ScopeArtifactsRead)
	}
	switch in.Name {
	case a2uiActionOpenMember:
		bundleID, _ := in.Context["bundle"].(string)
		memberName, _ := in.Context["member"].(string)
		if bundleID == "" || memberName == "" {
			return nil, nil, fmt.Errorf("validation_failed: open_member requires context.bundle and context.member")
		}
		out, err := s.renderMemberA2UI(ctx, bundleID, memberName)
		if err != nil {
			return nil, nil, s.mcpToolErr(ctx, "a2ui_action", err)
		}
		uri := "cairn://bundle/" + bundleID + "/" + a2uiMemberURIEscape(memberName) + "/a2ui"
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      uri,
					MIMEType: a2uiMIME,
					Text:     string(out),
				},
				Annotations: a2uiAudienceUser,
			},
			&mcp.TextContent{Text: fmt.Sprintf("Opened bundle member %q from %s.", memberName, bundleID)},
		}}, nil, nil
	default:
		return nil, nil, fmt.Errorf("validation_failed: unknown a2ui action %q", in.Name)
	}
}

// mcpA2UIErrorInput is the a2ui_error tool's input: the host reports a render
// failure on an MCP-served surface.
type mcpA2UIErrorInput struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SurfaceID string `json:"surfaceId"`
}

// mcpA2UIError is the a2ui_error tool handler: a sink for host-side render
// failures on cairn's A2UI surfaces. Registered so the host's "server exposes
// a2ui_error" capability check passes; cairn only logs — there is nothing to
// act on server-side. No scope gate beyond the session's: a render error about
// a surface the host was already shown carries no new authority.
func (s *Server) mcpA2UIError(ctx context.Context, req *mcp.CallToolRequest, in mcpA2UIErrorInput) (*mcp.CallToolResult, any, error) {
	s.log.WarnContext(ctx, "mcp: a2ui render error reported by host",
		"code", in.Code, "message", in.Message, "surface", in.SurfaceID)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "ok"},
	}}, nil, nil
}
