package httpapi

// Cross-cutting regression guards for the UX review remediation epic (#66).
// Each story landed its own per-story tests; this file pins the invariants
// that keep the fixes from regressing as a group, with every failure naming
// the story it guards (issue #74). Where a behavior is pure client-side JS
// (bin tab counts/scope, scrubber geometry), the guard is the plain-node unit
// test wired into `make test-js` — web/jstests/app_test.js and
// web/jstests/trajectory_test.js — referenced here so the two halves of the
// guard stay discoverable together. They sit in web/jstests/ rather than beside
// the code because web/assets/ is embedded and served at /assets/*, which
// TestAssetsShipNoTestFiles pins.

import (
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/sharetype"
)

// #67 / #136 — The Bin lenses partition the bin instead of overlapping it.
// #67 gave the tabs live counts; #136 replaced the three overlapping scopes
// (Bin / Shared / From agents — which showed identical counts on any ordinary
// bin and filtered nothing) with the two axes that actually split a bin:
// visibility (All / Shared / Private, a true complement) and share type. The
// template half (count slot + the visibility labels + the type menu) is
// asserted here; the JS half (counts, type-menu math, URL-hash round-trip) is
// pinned by app_test.js under `make test-js`.
func TestUXBinLensesPartitionTheBin(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{Actor: "joe"})
	for _, frag := range []string{
		`data-tab-count`, `data-scope="all"`, `data-scope="shared"`, `data-scope="private"`,
		">All <span", ">Shared <span", ">Private <span",
		`data-bin-types`, `data-bin-types-list`, `data-bin-types-reset`,
		`aria-label="Filter artifacts by type"`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("#136 bin lenses: missing %q", frag)
		}
	}
	// The overlapping agent lens is gone: it was never a partition (an agent
	// push is also shared), so it could only ever duplicate another tab.
	for _, gone := range []string{`data-scope="agents"`, ">From agents <span", ">Agents<"} {
		if strings.Contains(html, gone) {
			t.Errorf("#136 bin lenses: the overlapping %q lens should be gone", gone)
		}
	}
}

// #68 / ADR-0016 — One primary name for the flagship type, and it is "trace":
// the word users and agents already say, and the one ADR-0015's OTLP endpoint
// makes coherent. The badge is TRC and the display name is "trace"; "run"
// survives only as the EXECUTION a trace records, which is why the route and
// the MCP handle keep /run/. The registry key stays "trajectory" as a pure data
// hook until ADR-0016 Phase 2 flips the stored value — never reader-facing.
func TestUXTracesHaveOnePrimaryName(t *testing.T) {
	s := newWebServer(t)
	trj := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	for _, frag := range []string{
		`>TRC<`,                       // badge
		`title="trace"`,               // badge tooltip / reader-facing name
		`https://cairn.sh/run/abc123`, // route — a trace OF a run (ADR-0005 kept)
		`mcp://cairn/run/abc123`,      // mcp handle, likewise
		`data-type="trajectory"`,      // registry key stays the data hook only
	} {
		if !strings.Contains(trj, frag) {
			t.Errorf("ADR-0016 naming: trace shell missing %q", frag)
		}
	}
	// "trajectory" is a wire identifier, never a label: the only occurrence left
	// on the page is the data-type hook.
	if strings.Contains(trj, `title="trajectory"`) {
		t.Error("ADR-0016 naming: badge tooltip should read \"trace\", not the registry key")
	}
	// And the retired name must not come back as the reader-facing one.
	if strings.Contains(trj, `title="run"`) {
		t.Error("ADR-0016 naming: the type is a trace; \"run\" names the execution, not the artifact")
	}

	// The registry resolves the display name from data, not a switch (ADR-0002).
	if got := sharetype.Default().DisplayNameFor(sharetype.KeyTrajectory); got != "trace" {
		t.Errorf("ADR-0016 naming: DisplayNameFor(trajectory) = %q, want \"trace\"", got)
	}
}

// #69 — No pseudo-version suffix renders in row metadata; the full string
// rides in a tooltip. (The exhaustive truncation table is TestTruncateAgentVersion.)
func TestUXBinRowTruncatesPseudoVersion(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{
		Actor: "joe",
		Rows: []binRow{{
			ID: "abc123", Badge: "MD", TypeLabel: "markdown", TypeName: "markdown",
			Title: "notes", WebURL: "/abc123",
			Lead: "crush/v0.66.2 (build 123)", LeadShort: "crush/v0.66.2",
			Channel: "via MCP", Age: "2h ago",
		}},
	})
	if !strings.Contains(html, `title="crush/v0.66.2 (build 123)"`) {
		t.Error("#69 pseudo-version: full string should ride in the tooltip")
	}
	if !strings.Contains(html, ">crush/v0.66.2<") {
		t.Error("#69 pseudo-version: truncated form should render as the visible text")
	}
	if strings.Contains(html, ">crush/v0.66.2 (build 123)<") {
		t.Error("#69 pseudo-version: the suffix should not appear in the visible text")
	}
}

// #70 — Settings, consent, login, and error pages render with the app theme
// tokens (dark color-scheme + the app.css theme link via doc-head), and the
// agent-sessions empty state carries its scope clause.
func TestUXPagesShareAppTheme(t *testing.T) {
	s := newWebServer(t)
	// Every simple page composes doc-head, which carries the theme: dark
	// color-scheme and the app.css token sheet. Render each and assert both.
	for _, page := range []string{"login", "consent", "error", "settings", "landing"} {
		var sb strings.Builder
		if err := s.webTmpl.ExecuteTemplate(&sb, page, nil); err != nil {
			t.Fatalf("#70 theme: render %s: %v", page, err)
		}
		html := sb.String()
		if !strings.Contains(html, `name="color-scheme" content="dark"`) {
			t.Errorf("#70 theme: %s should declare the dark color-scheme token", page)
		}
		if !strings.Contains(html, `/assets/app.css`) {
			t.Errorf("#70 theme: %s should pull in the app.css theme tokens", page)
		}
	}

	// The sessions empty state names its scope: only OAuth agent sessions
	// appear, so token/CLI pushes are not implied to be missing (#70).
	var sb strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&sb, "settings", nil); err != nil {
		t.Fatalf("#70 theme: render settings: %v", err)
	}
	if !strings.Contains(sb.String(), "don't appear here") {
		t.Error("#70 sessions: empty state should carry its scope clause")
	}
}

// #71 — The push-help control is not primary-styled and not `+`-prefixed: it
// reads as help, not create.
func TestUXPushHelpReadsAsHelp(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{Actor: "joe"})
	if !strings.Contains(html, "how to push") {
		t.Error("#71 push help: the control should read as help (\"how to push\")")
	}
	if strings.Contains(html, "+ push") {
		t.Error("#71 push help: the control must not be `+`-prefixed (that read as create)")
	}
}

// #72 — The badge derives from content type, not push path, and every glyph
// carries a tooltip. The content→badge mapping is pinned exhaustively by
// TestDecidePreviewPromotesGenericFloorByContent in the sharetype package;
// this guards the bin-row glyph labels.
func TestUXBinRowGlyphsAreLabeled(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{
		Actor: "joe",
		Rows: []binRow{{
			ID: "abc123", Badge: "TRC", TypeLabel: "trajectory", TypeName: "trace",
			Title: "audit", WebURL: "/run/abc123",
			Lead: "claude", LeadShort: "claude", Channel: "via MCP", Age: "2h ago", TTL: "in 6d",
			ReactionCount: 1, CommentCount: 1, PinCount: 1,
		}},
	})
	for _, frag := range []string{
		`title="comments"`, `title="reactions"`, `title="image region pins"`,
		`title="live — tails in real time"`, `title="expires in 6d"`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("#72 glyphs: row missing labeled glyph %q", frag)
		}
	}
	if strings.Contains(html, `<span class="sr-only"> pins</span>`) {
		t.Error("#72 glyphs: the bare \"pins\" label should be \"image region pins\"")
	}
}

// #72 (content-derived badge) — the cross-cutting half: the same body badged
// through the registry gets the same badge regardless of the declared floor.
func TestUXBadgeDerivesFromContentType(t *testing.T) {
	r := sharetype.Default()
	const previewMax = 1 << 20
	// A markdown body is MD whether pushed declared-markdown or declared-file.
	for _, declared := range []artifact.ShareType{sharetype.KeyMarkdown, artifact.TypeFile, ""} {
		ty, _ := r.DecidePreview(declared, "text/markdown", 100, previewMax)
		if ty != sharetype.KeyMarkdown {
			t.Errorf("#72 badge: DecidePreview(%q, markdown) = %q, want markdown (badge by content, not push path)", declared, ty)
		}
	}
}

// #73 — Durations humanize. This guards the humanize helper the RUN STATS
// tiles are built from; the rendered tiles themselves (including the absent-
// token dash) are pinned by the trajectory integration suite.
func TestUXDurationsHumanize(t *testing.T) {
	for ms, want := range map[int64]string{
		2200:    "2.2s",   // below 90s: plain seconds
		260000:  "4m 20s", // 4m20s: humanized
		5550000: "1h 32m", // 1h32m: humanized, no seconds
		90000:   "1m 30s", // the 90s threshold humanizes
	} {
		if got := formatSeconds(ms); got != want {
			t.Errorf("#73 humanize: formatSeconds(%d) = %q, want %q", ms, got, want)
		}
	}
	// formatTokens is deliberately NOT where the dash lives — it is a pure
	// number formatter and renders 0 as "0". buildTrajectoryView substitutes
	// "—" plus a "not reported by this client" tooltip when the count is zero,
	// so a client that never reported tokens doesn't read as "used none".
	//
	// Pinned here because the substitution only makes sense while this stays
	// true: if formatTokens ever learned to return the dash itself, the tile
	// would be applying it twice and this test says where to look.
	if got := formatTokens(0); got != "0" {
		t.Errorf("#73 humanize: formatTokens(0) = %q, want \"0\" — the dash belongs to the tile, not the formatter", got)
	}
}
