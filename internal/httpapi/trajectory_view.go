package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/trajectory"
)

// The trajectory viewer (SPEC-0004, ADR-0011): the MVP's keystone body slot.
// Unlike a bodied share type — whose viewer renders from a single streamed body
// via the ADR-0002 BodyViewer capability — a trajectory is bodyless: its render
// data is the ordered span tree the trajectory service reconstructs from the
// spans table, plus derived run stats. The BodyViewer(io.Reader) seam cannot
// carry that tree, so the viewer resolves through the trajectory service the web
// server already holds (the same service the /v1/runs* surface projects). It is
// still driven by registry DATA, not a switch on the type key: the /run/{id}
// route is reachable only for artifacts whose registry web URL-prefix is "run"
// (URLPrefixFor), which only the trajectory type declares — exactly as the
// bare-scheme shell route owns the empty-prefix types (ADR-0002, ADR-0005).
//
// The whole page is server-rendered so the waterfall, the activity stream, the
// run stats, and the comment thread are all present and readable with no
// JavaScript (SPEC-0001 REQ "Progressive Enhancement"); trajectory.js only
// layers on the span↔stream cross-highlight, the reaction/comment pickers, and —
// for a live run — the SSE incremental span append and its aria-live
// announcements.
//
// Governing: ADR-0011 (server-rendered viewer fragments), ADR-0002 (registry
// resolution; no switch on type), ADR-0006 (unified annotation layer: reactions
// on turns/tool calls, comments on spans/selections), ADR-0007 (link-capability
// read, uniform 404), SPEC-0004 (Trajectory Share), SPEC-0001 (app shell,
// a11y), SPEC-0006 (registry-gated anchors).

// The five category swatch colors (reason #7D56F4 · exec #56E39F · read #6AA9FF
// · net #E3B341 · write #FF75B7, SPEC-0004 / design t7a legend) live in
// trajectory.css keyed on [data-cat], so both the server-rendered rows and the
// JS-appended live spans read one palette and Go never owns a rendered color.

// trajectoryView is the fully server-computed view model the trajectory template
// renders. Every field is derived from the run tree + the annotation service, so
// the page carries no client data layer (ADR-0011).
type trajectoryView struct {
	// Header chrome (shared shell contract).
	ID        string
	Badge     string
	Title     string
	TypeLabel string
	WebURL    string
	MCPHandle string
	MetaLine  string

	// Live state: an open run streams; a closed run is complete (SPEC-0004).
	Live bool

	// Waterfall header + geometry.
	Model         string
	SubAgentCount int
	ToolCallCount int
	Ticks         []rulerTick
	Waterfall     []waterfallRow

	// Activity stream (the user prompt turn followed by the span rows).
	Prompt      string
	PromptReact reactionCluster
	Stream      []streamRow

	// Right RUN panel.
	Provenance provenanceLine
	ExpiresIn  string
	Stats      []statTile
	Categories []categorySeg
	TokenCount string

	// Comments + composer gating.
	Comments      []commentLine
	CommentCount  int
	ReactionCount int
	Authenticated bool
	Actor         string
	CSRFToken     string

	// ShareDialog is the Share button's view model (#46), the same shared
	// partial the generic shell renders (SPEC-0001 REQ "Share Affordance":
	// "Works on both the generic shell and the trajectory viewer").
	ShareDialog shareDialogView
}

// rulerTick is one time-axis label at a fractional position across the track.
type rulerTick struct {
	Label string
	Pct   string
}

// waterfallRow is one span bar in the pinned waterfall, positioned by percentage
// of wall time so the same run lays out identically every render (SPEC-0004
// "Deterministic layout").
type waterfallRow struct {
	SpanID     string
	Name       string
	Category   string
	Tool       string
	Depth      int
	IsChild    bool
	IsSubAgent bool
	// StartMS/DurMS are the raw offsets; trajectory.js lays out every bar's
	// left/width from these against the wall time it derives as max(start+dur),
	// so the initial render and a live-appended span share one layout path and a
	// late span that extends the timeline re-proportions the whole waterfall.
	StartMS  int
	DurMS    int
	Duration string
}

// streamRow is one activity-stream turn: a role marker + a collapsible card. A
// sub-agent row nests its child rows. Each row may carry a reaction cluster
// (turn or tool-call anchor) and is comment-targetable on its span anchor.
type streamRow struct {
	SpanID   string
	Role     string // user | assistant | reason | tool | sub-agent
	Category string
	Tool     string
	Name     string
	Meta     string // "· 2.9s", "· 2 tools · 10.6s"
	// DurMS is the span's own duration, rendered as data-dur-ms so
	// trajectory.js can recompute a sub-agent's "N tools · Xs" Meta line when a
	// live child streams in under it, without re-parsing the rendered Meta text
	// (#38 review note 2).
	DurMS int
	Body  string // inline output / reasoning prose (escaped by the template)
	// Spilled reports the body lives in a content-addressed blob fetched lazily
	// on expand (SPEC-0004 "Large outputs MUST be fetched lazily"); OutputURL is
	// where trajectory.js fetches it.
	Spilled       bool
	OutputURL     string
	OpenByDefault bool
	// Artifact cross-link (write span → produced markdown share, SPEC-0004
	// "Produced-Artifact Link").
	Artifact *artifactCard
	// Reaction anchor + current tallies. Turn rows anchor reactions to
	// trajectory_turn; tool/sub-agent/write rows to trajectory_toolcall.
	React    reactionCluster
	Children []streamRow
}

// artifactCard is the write→artifact cross-link (design t7a: MD chip + filename +
// SHARED badge, opening the produced markdown share).
type artifactCard struct {
	ID       string
	Filename string
	URL      string
}

// reactionCluster is a labeled group of reaction pills plus the anchor a new
// reaction attaches to. AnchorType/SpanID drive the /v1 reaction POST the picker
// issues; an empty SpanID means the whole-artifact anchor.
type reactionCluster struct {
	AnchorType string
	SpanID     string
	Label      string // accessible group name, e.g. "Reactions on npm ls"
	Pills      []reactionPill
}

// reactionPill is one emoji tally on an anchor: its count and whether the viewer
// already reacted (the toggle state the picker flips).
type reactionPill struct {
	Emoji   string
	Count   int
	Reacted bool
}

// statTile is one RUN-STATS figure (2×2 grid): a value, its unit, and a label.
type statTile struct {
	Value string
	Unit  string
	Label string
}

// categorySeg is one segment of the TIME BY CATEGORY stacked bar and its legend
// row (SPEC-0004 "Derived Run Statistics": time-by-category sums the same span
// rows the waterfall draws).
type categorySeg struct {
	Category string
	Pct      string
	Duration string
}

// handleRunShell renders the trajectory viewer at GET /run/{id} (SPEC-0004,
// ADR-0011). It holds the same canonical-prefix discipline as the generic shell:
// the id must resolve AND its registry web prefix must be "run", else the same
// uniform 404 as an unknown id (ADR-0007). A storeless unit wiring (no
// trajectory service) falls back to the generic shell so URL/auth helper tests
// keep working without a database.
func (s *Server) handleRunShell(w http.ResponseWriter, r *http.Request) {
	if s.traj == nil {
		s.renderShellFor(w, r, "run")
		return
	}
	id := chi.URLParam(r, "id")
	a, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	if s.reg.URLPrefixFor(a.ShareType).Web != "run" {
		// Resolves, but not at this path: uniform 404 so each route owns exactly
		// its canonical prefix and leaks no signal (ADR-0007).
		s.renderWebError(w, r, errs.ErrNotFound)
		return
	}
	run, err := s.traj.GetRun(r.Context(), id)
	if err != nil {
		s.renderWebError(w, r, err)
		return
	}
	vm := s.buildTrajectoryView(r.Context(), a, run)
	if p, ok := s.optionalPrincipal(r); ok {
		vm.Authenticated = true
		vm.Actor = p.ActorID
		vm.ShareDialog.IsOwner = p.ActorID == a.Access.OwnerID
		if c, cerr := r.Cookie(csrfCookieName); cerr == nil {
			vm.CSRFToken = c.Value
		}
	}
	s.renderWeb(w, r, "trajectory", vm)
}

// buildTrajectoryView projects a run + its artifact envelope into the viewer
// model: the header facts, the waterfall geometry, the activity stream, the RUN
// panel stats, and the annotation state (reaction tallies keyed by anchor, the
// comment thread). Stats come from run.Stats (derived from the same span rows
// the waterfall draws), so the panel and the waterfall can never disagree
// (SPEC-0004 "Derived Run Statistics").
func (s *Server) buildTrajectoryView(ctx context.Context, a *artifact.Artifact, run *trajectory.Run) trajectoryView {
	wall := run.Stats.WallTimeMS
	if wall <= 0 {
		wall = 1 // avoid divide-by-zero on a just-opened run with no spans
	}

	vm := trajectoryView{
		ID:            a.PublicID,
		Badge:         s.reg.BadgeFor(a),
		Title:         firstNonEmpty(run.Title, a.PublicID),
		TypeLabel:     string(a.ShareType),
		WebURL:        s.webURL(a),
		MCPHandle:     s.mcpHandle(a),
		MetaLine:      itoa(run.Stats.SpanCount) + " spans · " + formatSeconds(wall),
		Live:          run.Status == trajectory.StatusOpen,
		Model:         run.Model,
		ToolCallCount: run.Stats.ToolCallCount,
		Prompt:        run.Prompt,
		TokenCount:    formatTokens(run.Stats.TokenCount),
		ReactionCount: a.ReactionCount,
		CommentCount:  a.CommentCount,
		Provenance: provenanceLine{
			Actor:      run.Provenance.ActorID,
			OnBehalfOf: run.Provenance.OnBehalfOf,
			Channel:    string(run.Provenance.Channel),
			Captured:   humanizeSince(run.Provenance.CapturedAt),
		},
		ExpiresIn: humanizeUntil(run.ExpiresAt),
	}
	vm.Provenance.Expires = vm.ExpiresIn
	vm.ShareDialog = shareDialogView{
		WebURL:      vm.WebURL,
		MCPHandle:   vm.MCPHandle,
		AccessLabel: shareAccessLabel(a.Access.Visibility),
		ExpiresIn:   vm.ExpiresIn,
		Provenance:  vm.Provenance,
	}

	// Time ruler: five ticks 0→wall at quartile positions (SPEC-0004 waterfall
	// time axis).
	for _, q := range []struct {
		pct float64
	}{{0}, {25}, {50}, {75}, {100}} {
		vm.Ticks = append(vm.Ticks, rulerTick{
			Label: formatSeconds(int64(float64(wall) * q.pct / 100)),
			Pct:   formatPct(q.pct),
		})
	}

	// Reaction tallies, grouped by anchor (type|key). An anonymous read gets
	// all-false "reacted" flags; a signed-in viewer gets their own toggle state.
	tallies := s.reactionIndex(ctx, a.PublicID)

	// Waterfall rows: a depth-first walk of the ordered tree so a sub-agent's
	// children render directly beneath it, indented (design t7a nesting).
	vm.SubAgentCount = 0
	var walkWaterfall func(spans []*trajectory.Span)
	walkWaterfall = func(spans []*trajectory.Span) {
		for _, sp := range spans {
			if sp.Tool == "sub-agent" {
				vm.SubAgentCount++
			}
			vm.Waterfall = append(vm.Waterfall, waterfallRow{
				SpanID:     sp.SpanID,
				Name:       sp.Name,
				Category:   string(sp.Category),
				Tool:       sp.Tool,
				Depth:      sp.Depth,
				IsChild:    sp.Depth > 0,
				IsSubAgent: sp.Tool == "sub-agent",
				StartMS:    sp.StartOffsetMS,
				DurMS:      sp.DurationMS,
				Duration:   formatSeconds(int64(sp.DurationMS)),
			})
			walkWaterfall(sp.Children)
		}
	}
	walkWaterfall(run.Spans)

	// The user prompt is the run's opening turn — not a span, so its reactions
	// pin to the whole-artifact anchor (a valid trajectory reaction anchor).
	vm.PromptReact = reactionCluster{
		AnchorType: string(sharetype.AnchorArtifact),
		Label:      "Reactions on the prompt",
		Pills:      tallies[string(sharetype.AnchorArtifact)+"|{}"],
	}

	// Activity stream: one row per top-level span, sub-agents nesting their
	// children (design t7a stream).
	for _, sp := range run.Spans {
		vm.Stream = append(vm.Stream, s.streamRowFor(a.PublicID, sp, tallies))
	}

	// RUN STATS 2×2 tiles (all derived).
	vm.Stats = []statTile{
		{Value: formatSecondsBare(wall), Unit: "s", Label: "wall time"},
		{Value: itoa(run.Stats.SpanCount), Label: "spans"},
		{Value: itoa(run.Stats.ToolCallCount), Label: "tool calls"},
		{Value: formatTokens(run.Stats.TokenCount), Label: "tokens"},
	}

	// TIME BY CATEGORY stacked bar, in the legend's fixed order.
	for _, cat := range []string{"reason", "net", "exec", "read", "write"} {
		ms := run.Stats.TimeByCategoryMS[trajectory.Category(cat)]
		if ms == 0 {
			continue
		}
		vm.Categories = append(vm.Categories, categorySeg{
			Category: cat,
			Pct:      formatPct(ratioPct(int(ms), wall)),
			Duration: formatSeconds(ms),
		})
	}

	if s.annot != nil {
		if comments, err := s.annot.ListComments(ctx, a.PublicID); err == nil {
			vm.Comments = toCommentLines(comments)
		} else {
			s.log.WarnContext(ctx, "web: trajectory list comments failed", "id", a.PublicID, "error", err)
		}
	}
	return vm
}

// streamRowFor projects one span onto an activity-stream row, recursively for a
// sub-agent's children. It picks the reaction anchor from the span's shape — a
// tool-bearing span (including a sub-agent or a write) reacts on
// trajectory_toolcall, a bare reasoning turn on trajectory_turn — matching the
// SPEC-0006 capability matrix.
func (s *Server) streamRowFor(publicID string, sp *trajectory.Span, tallies map[string][]reactionPill) streamRow {
	row := streamRow{
		SpanID:   sp.SpanID,
		Category: string(sp.Category),
		Tool:     sp.Tool,
		Name:     sp.Name,
		Meta:     "· " + formatSeconds(int64(sp.DurationMS)),
		DurMS:    sp.DurationMS,
		Body:     sp.Inline,
	}
	switch {
	case sp.Tool == "sub-agent":
		row.Role = "sub-agent"
		row.OpenByDefault = true
		row.Meta = "· " + itoa(len(sp.Children)) + " tools · " + formatSeconds(int64(sp.DurationMS))
	case sp.Tool != "":
		row.Role = "tool"
		// Exec + write cards read best open; reads collapse (design t7a).
		row.OpenByDefault = sp.Category == "exec" || sp.Category == "write"
	default:
		row.Role = "reason"
	}

	if sp.Ref != nil {
		row.Spilled = true
		row.Body = ""
		row.OutputURL = "/v1/runs/" + publicID + "/spans/" + sp.SpanID + "/output"
	}

	// Reaction anchor + tallies.
	anchorType := string(sharetype.AnchorTrajectoryTurn)
	if sp.Tool != "" {
		anchorType = string(sharetype.AnchorTrajectoryToolCall)
	}
	key := spanAnchorKey(sp.SpanID)
	row.React = reactionCluster{
		AnchorType: anchorType,
		SpanID:     sp.SpanID,
		Label:      "Reactions on " + firstNonEmpty(sp.Name, sp.SpanID),
		Pills:      tallies[anchorType+"|"+key],
	}

	// Write → produced-artifact cross-link card.
	if len(sp.ProducedArtifactIDs) > 0 {
		pid := sp.ProducedArtifactIDs[0]
		row.Artifact = &artifactCard{
			ID:       pid,
			Filename: firstNonEmpty(sp.Name, pid),
			URL:      "/" + pid,
		}
	}

	for _, child := range sp.Children {
		row.Children = append(row.Children, s.streamRowFor(publicID, child, tallies))
	}
	return row
}

// reactionIndex loads the run's reaction tallies and buckets them by
// "anchorType|anchorKey", so the viewer can hang each span's pills on its own
// anchor. actorID drives the "did I react" toggle state; an anonymous read
// leaves every pill un-reacted.
func (s *Server) reactionIndex(ctx context.Context, publicID string) map[string][]reactionPill {
	out := map[string][]reactionPill{}
	if s.annot == nil {
		return out
	}
	tallies, err := s.annot.ReactionTallies(ctx, publicID, "")
	if err != nil {
		s.log.WarnContext(ctx, "web: trajectory reaction tallies failed", "id", publicID, "error", err)
		return out
	}
	for _, t := range tallies {
		k := string(t.AnchorType) + "|" + t.AnchorKey
		out[k] = append(out[k], reactionPill{Emoji: t.Emoji, Count: t.Count, Reacted: t.Reacted})
	}
	return out
}

// spanAnchorKey is the canonical anchor_key for a trajectory span/turn/toolcall
// locator {"span_id":"..."} — encoding/json emits sorted keys with no
// whitespace, which is exactly the canonical form the annotation service stores
// (see annotation.CanonicalRef), so the server-rendered tally key matches the
// stored one without re-deriving it.
func spanAnchorKey(spanID string) string {
	b, _ := json.Marshal(struct {
		SpanID string `json:"span_id"`
	}{spanID})
	return string(b)
}

// --- formatting helpers ----------------------------------------------------

// ratioPct returns part/whole as a percentage in [0,100].
func ratioPct(part int, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	p := float64(part) / float64(whole) * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// formatPct renders a percentage with one decimal place (e.g. "31.0"), the unit
// the template feeds straight into a `left`/`width` style.
func formatPct(p float64) string {
	return strconv.FormatFloat(p, 'f', 1, 64)
}

// formatSeconds renders milliseconds as one-decimal seconds with a trailing "s"
// (e.g. 2900 → "2.9s", 34164 → "34.2s"), matching the design's duration labels.
func formatSeconds(ms int64) string {
	return formatSecondsBare(ms) + "s"
}

// formatSecondsBare is formatSeconds without the unit, for a tile that renders
// the unit separately.
func formatSecondsBare(ms int64) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64)
}

// formatTokens renders a token count compactly (48100 → "48.1k", 900 → "900").
func formatTokens(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
}
