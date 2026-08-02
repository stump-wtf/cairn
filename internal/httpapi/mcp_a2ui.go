// A2UI (Agent-to-UI) resources: render `application/a2ui+json` projections of
// a trajectory run's waterfall + stream, and a bundle's member cards, so an
// A2UI-capable MCP host (Crush once joestump-agent/crush#217 lands) draws the
// view inline instead of asking the model to re-render raw JSON.
//
// Two resources ship in v1, both read-only at the A2UI layer (mutations stay
// on the existing MCP tools; the buttons on these surfaces are placeholders
// until the `a2ui_action` round-trip lands in joestump-agent/crush#221):
//
//	cairn://run/{id}/a2ui     — trace header + span waterfall + stats
//	cairn://bundle/{id}/a2ui  — bundle envelope + one Card per member
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
//	Card "stats"   — wall time, span count, tool calls, tokens, top categories
//	Card "spans"   — one row per span, indented by depth, with category,
//	                 tool/name, duration; expandable per-span output stays
//	                 behind the existing JSON resource (lazy by design)
//
// Buttons (close, react) render with action names so a host that supports
// a2ui_action can wire them up, but Cairn's MCP surface ignores them until
// the round-trip is real (joestump-agent/crush#221). The tree is capped at
// a2uiMaxRunSpans components — a run that big should be paged over the
// existing run JSON resource, not squeezed into a single surface.
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

	// Stats card — small caption row of derived facts, plus a category
	// breakdown when there is more than one category to show.
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

	if len(stats.TimeByCategoryMS) > 0 {
		type catEntry struct {
			name string
			ms   int64
		}
		cats := make([]catEntry, 0, len(stats.TimeByCategoryMS))
		for name, ms := range stats.TimeByCategoryMS {
			cats = append(cats, catEntry{name, ms})
		}
		sort.Slice(cats, func(i, j int) bool { return cats[i].ms > cats[j].ms })
		const maxCats = 5
		if len(cats) > maxCats {
			cats = cats[:maxCats]
		}
		parts := make([]string, 0, len(cats))
		for _, c := range cats {
			parts = append(parts, fmt.Sprintf("%s %s", c.name, a2uiMS(c.ms)))
		}
		statsChildren = append(statsChildren, "stats-cats")
		components = append(components, a2uiText("stats-cats", strings.Join(parts, " · "), "caption"))
	}

	components = append(components,
		a2uiColumn("stats-col", statsChildren),
		a2uiCard("stats", "stats-col"),
	)

	// Spans card — a flat list, indented by depth. The web UI's waterfall
	// bar chart is a richer encoding than the catalog's primitives; here the
	// span name + category + duration carry the same information in the
	// format the TUI can render.
	spanIDs := []string{}
	spanCount := 0
	var walk func(spans []spanView)
	walk = func(spans []spanView) {
		for _, sp := range spans {
			if spanCount >= a2uiMaxRunSpans {
				return
			}
			spanCount++
			id := fmt.Sprintf("span-%d", spanCount)
			label := a2uiIndent(sp.Depth)
			if sp.Category != "" {
				label += "[" + sp.Category + "] "
			}
			if sp.Name != "" {
				label += sp.Name
			} else if sp.Tool != "" {
				label += sp.Tool
			} else {
				label += sp.SpanID
			}
			if sp.DurationMS > 0 {
				label += " · " + a2uiMS(int64(sp.DurationMS))
			}
			spanIDs = append(spanIDs, id)
			components = append(components, a2uiText(id, label, "body"))
			walk(sp.Children)
		}
	}
	walk(resp.Spans)
	if len(resp.Spans) > 0 && spanCount >= a2uiMaxRunSpans {
		spanIDs = append(spanIDs, "span-overflow")
		components = append(components,
			a2uiText("span-overflow",
				fmt.Sprintf("…%d more spans (read the JSON resource for the full tree)", stats.SpanCount-spanCount),
				"caption"))
	}

	if len(spanIDs) == 0 {
		spanIDs = append(spanIDs, "span-empty")
		components = append(components, a2uiText("span-empty", "no spans yet", "caption"))
	}

	components = append(components,
		a2uiList("spans-list", spanIDs),
		a2uiCard("spans", "spans-list"),
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

// a2uiBundleView renders a bundle as an envelope header Card plus one Card
// per member (name, size, media type). Like the trace view, actions on the
// cards are wired for a2ui_action but inert until the round-trip lands.
const a2uiMaxBundleMembers = 100

func a2uiBundleView(bundleID string, art *artifact.Artifact, members []mcpMemberView) a2uiEnvelope {
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
	metaBits := []string{
		fmt.Sprintf("%d members", len(members)),
		"visibility: " + string(art.Access.Visibility),
		"expires " + art.ExpiresAt.UTC().Format("2006-01-02"),
	}
	components = append(components,
		a2uiText("hdr-meta", strings.Join(metaBits, " · "), "caption"),
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
		cardID := fmt.Sprintf("member-%d", i)
		nameID := fmt.Sprintf("member-%d-name", i)
		metaID := fmt.Sprintf("member-%d-meta", i)

		components = append(components,
			a2uiText(nameID, m.Name, "h5"),
			a2uiText(metaID,
				fmt.Sprintf("%s · %s", a2uiHumanBytes(m.Size), m.MediaType),
				"caption"),
			a2uiColumn(colID, []string{nameID, metaID}),
			a2uiCard(cardID, colID),
		)
		memberIDs = append(memberIDs, cardID)
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
			a2uiColumn("members-col", memberIDs),
			a2uiCard("members", "members-col"),
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
// components. Reading requires artifacts:read.
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
	members := make([]mcpMemberView, 0, len(memberRows))
	for _, m := range memberRows {
		members = append(members, mcpMemberView{Name: m.Name, Size: m.Size, MediaType: m.MediaType})
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
