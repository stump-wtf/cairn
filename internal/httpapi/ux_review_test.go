package httpapi

// Cross-cutting regression guards for the UX review remediation epic (#66).
// Each story landed its own per-story tests; this file pins the invariants
// that keep the fixes from regressing as a group, with every failure naming
// the story it guards (issue #74). Where a behavior is pure client-side JS
// (bin tab counts/scope, scrubber geometry), the guard is the plain-node unit
// test wired into `make test-js` (app_js_test.js / trajectory_js_test.js) —
// referenced here so the two halves of the guard stay discoverable together.

import (
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/sharetype"
)

// #67 — Bin tabs show counts, scope restores from the URL, per-scope empty
// states exist. The template half (count slot + truthful "From agents" label)
// is asserted here; the JS half (count computation, URL-hash restoration,
// per-scope empty copy) is pinned by app_js_test.js under `make test-js`.
func TestUXBinTabsRenderCountsAndTruthfulLabels(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{Actor: "joe"})
	for _, frag := range []string{
		`data-tab-count`, `data-scope="all"`, `data-scope="shared"`, `data-scope="agents"`,
		">From agents <span", // the truthful label, not "Agents" (#67)
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("#67 bin tabs: missing %q", frag)
		}
	}
	if strings.Contains(html, ">Agents<") {
		t.Error("#67 bin tabs: the misleading \"Agents\" label should be gone")
	}
}

// #68 — One primary name for the flagship type: "run" across badge, bin label,
// detail title, and route. The badge is RUN, the display name is "run", the
// route is /run/, and the registry key stays "trajectory" only as the data
// hook (never reader-facing). No legacy /trajectory/ route exists to break.
func TestUXRunsHaveOnePrimaryName(t *testing.T) {
	s := newWebServer(t)
	trj := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	for _, frag := range []string{
		`>RUN<`,                       // badge
		`title="run"`,                 // badge tooltip / reader-facing name
		`https://cairn.sh/run/abc123`, // route
		`mcp://cairn/run/abc123`,      // mcp handle
		`data-type="trajectory"`,      // registry key stays the data hook only
	} {
		if !strings.Contains(trj, frag) {
			t.Errorf("#68 run naming: trajectory shell missing %q", frag)
		}
	}
	// The reader-facing name is "run" everywhere a reader meets it; the only
	// "trajectory" left on the page is the data-type hook, never a label.
	if strings.Contains(trj, `title="trajectory"`) {
		t.Error("#68 run naming: badge tooltip should read \"run\", not the registry key")
	}

	// The registry resolves the display name from data, not a switch (ADR-0002).
	if got := sharetype.Default().DisplayNameFor(sharetype.KeyTrajectory); got != "run" {
		t.Errorf("#68 run naming: DisplayNameFor(trajectory) = %q, want \"run\"", got)
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
	if strings.Contains(html, ">+ push<") || strings.Contains(html, "+ push") {
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
			ID: "abc123", Badge: "RUN", TypeLabel: "trajectory", TypeName: "run",
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

// #73 — Durations humanize; an absent stat renders "—", not a fabricated 0.
// The integration suite pins the rendered tiles; this guards the humanize
// helpers the tiles are built from.
func TestUXDurationsHumanizeAndAbsentStatsDash(t *testing.T) {
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
	// An absent token count is "—" with an explanatory tooltip, never "0"
	// (which would read as "no tokens used").
	if got := formatTokens(0); got != "0" {
		t.Errorf("#73 humanize: formatTokens(0) = %q, want \"0\" (the dash is applied at the tile, not here)", got)
	}
}
