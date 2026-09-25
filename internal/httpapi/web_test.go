package httpapi

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// newWebServer builds a Server with no store (so buildShellView exercises only
// the registry + template path, never the DB): enough to assert the shell's
// server-rendered HTML for any share type.
func newWebServer(t *testing.T) *Server {
	t.Helper()
	return New(nil, sharetype.Default(), nil, Config{BaseURL: "https://cairn.sh"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func fixtureArtifact(shareType artifact.ShareType, hasBody bool) *artifact.Artifact {
	a := &artifact.Artifact{
		PublicID:  "abc123",
		ShareType: shareType,
		Title:     "checkout-web-audit",
		Size:      1234,
		MediaType: "text/markdown",
		Provenance: artifact.Provenance{
			ActorID: "joe", Channel: artifact.ChannelMCP, CapturedAt: time.Now().Add(-2 * time.Hour),
		},
		Access:    artifact.AccessPolicy{OwnerUserID: "joe", Visibility: artifact.VisibilityLink},
		ExpiresAt: time.Now().Add(5 * 24 * time.Hour),
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	if hasBody {
		a.BodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}
	return a
}

func renderShellHTML(t *testing.T, s *Server, a *artifact.Artifact) string {
	t.Helper()
	vm := s.buildShellView(context.Background(), a, "")
	var sb strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&sb, "shell", vm); err != nil {
		t.Fatalf("render shell (%s): %v", a.ShareType, err)
	}
	return sb.String()
}

// TestShellChromeIsIdenticalAcrossTypes is the ADR-0011 shell-consistency
// invariant: two artifacts of different share types render the SAME header
// structure (logo · badge · title · one URL control with copy + ◆ mcp · Share ·
// panel toggle) and the same collapsible panel, with only the badge/title/URL
// and body varying (SPEC-0001 REQ "Unified App Shell", REQ "Header Composition").
func TestShellChromeIsIdenticalAcrossTypes(t *testing.T) {
	s := newWebServer(t)

	// The chrome fragments every type must present, verbatim. `>Comments `
	// (the panel's Comments section heading) is asserted separately below,
	// per type: webhook is the one registered type whose registry entry
	// declares NO comment anchors at all (SPEC-0006 REQ "Webhook
	// Reaction-Only Asymmetry"), so its shell hides the whole Comments
	// section — CommentsSupported gates it off the SAME registry data this
	// invariant is about, never a type-key switch (issue #86).
	chrome := []string{
		`role="banner"`,       // banner landmark (header)
		`class="url-control"`, // exactly one URL control
		`aria-label="Copy URL to clipboard"`,
		`◆ mcp`,                             // mcp affordance
		`class="share-btn"`,                 // Share button
		`aria-expanded`,                     // panel toggle exposes state
		`<main`,                             // main landmark
		`aria-label="Details and comments"`, // panel
		`>Provenance<`,                      // panel provenance section
	}

	types := []artifact.ShareType{
		artifact.TypeFile,
		sharetype.KeyMarkdown,
		sharetype.KeyImage,
		sharetype.KeyWebhook,
		artifact.TypeTrajectory,
	}
	for _, tp := range types {
		html := renderShellHTML(t, s, fixtureArtifact(tp, tp != artifact.TypeTrajectory))
		for _, frag := range chrome {
			if !strings.Contains(html, frag) {
				t.Errorf("share type %q: shell missing required chrome fragment %q", tp, frag)
			}
		}
		if tp == sharetype.KeyWebhook {
			if strings.Contains(html, `>Comments `) {
				t.Errorf("share type %q: shell must expose NO comment affordance (SPEC-0006 Webhook Reaction-Only Asymmetry), but found the Comments section", tp)
			}
		} else if !strings.Contains(html, `>Comments `) {
			t.Errorf("share type %q: shell missing required chrome fragment %q", tp, `>Comments `)
		}
		// Exactly one URL control across every type.
		if n := strings.Count(html, `class="url-control"`); n != 1 {
			t.Errorf("share type %q: want exactly one URL control, got %d", tp, n)
		}
	}
}

// TestShareDialogSkeletonRenders asserts the Share dialog (#46, SPEC-0001 REQ
// "Share Affordance") is present in the server-rendered HTML for every share
// type: a native <dialog> carrying the web link + mcp handle each with their
// own copy affordance, the access-policy line, the expiry summary, and the
// provenance line — all sourced from the same server-computed facts as the
// header/panel, never re-derived client-side.
func TestShareDialogSkeletonRenders(t *testing.T) {
	s := newWebServer(t)

	types := []artifact.ShareType{
		artifact.TypeFile,
		sharetype.KeyMarkdown,
		artifact.TypeTrajectory,
	}
	for _, tp := range types {
		html := renderShellHTML(t, s, fixtureArtifact(tp, tp != artifact.TypeTrajectory))
		for _, frag := range []string{
			`<dialog class="share-dialog" data-share-dialog data-share-id="`,
			`aria-labelledby="share-dialog-title">`,
			`id="share-dialog-title"`,
			`aria-label="Close share dialog"`,
			`aria-label="Copy web link to clipboard"`,
			`aria-label="Copy MCP handle to clipboard"`,
			`https://cairn.sh/`,            // the web link value inside the dialog
			`mcp://cairn/`,                 // the mcp handle value inside the dialog
			`🔒 you &#43; anyone with link`, // ADR-0007 default access line (html/template escapes "+")
			`⧗ expires`,                    // TTL/expiry summary
			`via MCP · captured`,           // provenance line
		} {
			if !strings.Contains(html, frag) {
				t.Errorf("share type %q: share dialog missing %q", tp, frag)
			}
		}
		// Exactly one Share dialog per page.
		if n := strings.Count(html, `data-share-dialog`); n != 1 {
			t.Errorf("share type %q: want exactly one share dialog, got %d", tp, n)
		}
	}
}

// TestShareDialogOwnerNote asserts the dialog shows a non-owner/signed-out
// note — and NO mutating control whatsoever — when the viewer is not the
// owner, and the reverse (owner controls present, no note) for the owner
// (issue #94, SPEC-0001 REQ "Share Affordance": "A non-owner or
// unauthenticated viewer MUST NOT be able to change sharing"; SPEC-0009 REQ
// "Owner-Only Policy Changes").
func TestShareDialogOwnerNote(t *testing.T) {
	s := newWebServer(t)
	a := fixtureArtifact(artifact.TypeFile, true)

	nonOwner := s.buildShellView(context.Background(), a, "")
	var sb strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&sb, "shell", nonOwner); err != nil {
		t.Fatalf("render shell: %v", err)
	}
	html := sb.String()
	if !strings.Contains(html, "Only the owner can change this artifact's sharing policy.") {
		t.Error("non-owner/signed-out viewer should see the owner-only note")
	}
	for _, absent := range []string{`data-share-owner-controls`, `data-share-ttl-save`, `data-share-rotate`, `data-share-visibility-save`} {
		if strings.Contains(html, absent) {
			t.Errorf("non-owner/signed-out viewer must not see owner control %q", absent)
		}
	}

	owner := s.buildShellView(context.Background(), a, "")
	owner.ShareDialog.IsOwner = true
	sb.Reset()
	if err := s.webTmpl.ExecuteTemplate(&sb, "shell", owner); err != nil {
		t.Fatalf("render shell: %v", err)
	}
	html = sb.String()
	if strings.Contains(html, "Only the owner can change") {
		t.Error("owner should not see the owner-only note")
	}
	for _, present := range []string{`data-share-owner-controls`, `data-share-ttl-save`, `data-share-rotate`, `data-share-visibility-save`, `data-confirm="Rotate`} {
		if !strings.Contains(html, present) {
			t.Errorf("owner should see owner control %q", present)
		}
	}
}

// TestShellBadgeAndURLVaryByType asserts the two header data points that DO
// vary: the registry badge and the ADR-0005 URL scheme (a trajectory carries
// the /run/ sub-prefix on both the web link and the mcp handle).
func TestShellBadgeAndURLVaryByType(t *testing.T) {
	s := newWebServer(t)

	md := renderShellHTML(t, s, fixtureArtifact(sharetype.KeyMarkdown, true))
	if !strings.Contains(md, `https://cairn.sh/abc123`) {
		t.Error("markdown web URL should be the bare id path")
	}
	if !strings.Contains(md, `>MD<`) {
		t.Error("markdown badge should be MD")
	}

	trj := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	if !strings.Contains(trj, `https://cairn.sh/run/abc123`) {
		t.Error("trajectory web URL should carry the /run/ prefix")
	}
	if !strings.Contains(trj, `mcp://cairn/run/abc123`) {
		t.Error("trajectory mcp handle should carry the /run/ prefix")
	}
	if !strings.Contains(trj, `>TRC<`) {
		t.Error("trajectory badge should be TRC — a badge names what the artifact holds (a trace)")
	}
	if !strings.Contains(trj, `title="trace"`) {
		t.Error("trace badge tooltip should read \"trace\" (ADR-0016)")
	}
}

// TestShellGenericBodyFloor asserts the total-resolution floor (#12): a type
// with no registered BodyViewer renders the generic-file card (SPEC-0001 REQ
// "Type-Specific Body Slot") carrying the file glyph, size, and sha256, plus a
// same-origin sniff-proof `/{id}/download` link when the body exists. A bodyless
// type gets the card but no download.
func TestShellGenericBodyFloor(t *testing.T) {
	s := newWebServer(t)

	withBody := renderShellHTML(t, s, fixtureArtifact(artifact.TypeFile, true))
	for _, frag := range []string{
		`class="generic-card"`,     // the floor card
		`class="file-glyph"`,       // the file glyph
		`>1.2 kB<`,                 // the size
		`>e3b0c44298fc<`,           // the short sha256
		`href="/abc123/download"`,  // the same-origin, sniff-proof web download
		`class="file-badge">FILE<`, // the registry badge on the card
	} {
		if !strings.Contains(withBody, frag) {
			t.Errorf("generic-file floor missing %q", frag)
		}
	}
	// The floor links the web download route, never the /v1 API body path.
	if strings.Contains(withBody, `/v1/artifacts/`) {
		t.Error("the file card must link the web /{id}/download route, not the /v1 API")
	}

	bodyless := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	if !strings.Contains(bodyless, `class="generic-card"`) {
		t.Error("a bodyless type still resolves to the generic-file floor")
	}
	if strings.Contains(bodyless, `/download"`) {
		t.Error("a bodyless artifact must not expose a download link")
	}
}

// TestShellEscapesUserContent asserts the shell HTML-escapes user-controlled
// strings (title) — the CSP forbids inline execution and the template layer
// output-encodes, so an injected tag cannot break out (SPEC-0001 Security).
func TestShellEscapesUserContent(t *testing.T) {
	s := newWebServer(t)
	a := fixtureArtifact(artifact.TypeFile, true)
	a.Title = `<script>alert(1)</script>`
	html := renderShellHTML(t, s, a)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("user title must be HTML-escaped, not rendered as a live tag")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("user title should appear escaped in the output")
	}
}

// TestWebRoutesCarryScopedCSP asserts the per-surface CSP scoping: HTML shell
// routes (landing, assets) get webCSP while the /v1 API keeps the strict
// default-src 'none' — neither loosens the other (SPEC-0001 REQ "Security
// Headers", CSP scoped by route).
func TestWebRoutesCarryScopedCSP(t *testing.T) {
	s := newWebServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Web landing: web CSP, permits self scripts/styles for HTMX+Alpine.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != webCSP {
		t.Errorf("landing CSP = %q, want webCSP", got)
	}
	// The shell renders untrusted user content (titles, comments); the vendored
	// Alpine CSP build + eval-free template markup mean the policy must never
	// carry 'unsafe-eval' or 'unsafe-inline' on this surface.
	for _, bad := range []string{"'unsafe-eval'", "'unsafe-inline'"} {
		if strings.Contains(webCSP, bad) {
			t.Errorf("webCSP must not contain %s (weakens the user-content surface): %q", bad, webCSP)
		}
	}

	// An embedded asset is served under the web CSP too.
	resp, err = http.Get(srv.URL + "/assets/app.css")
	if err != nil {
		t.Fatalf("GET /assets/app.css: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("asset status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "--page: #0A0B0D") {
		t.Error("app.css should be served from the embedded FS")
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != webCSP {
		t.Errorf("asset CSP = %q, want webCSP", got)
	}

	// The /v1 API keeps the strict policy.
	resp, err = http.Get(srv.URL + "/v1/artifacts/none")
	if err != nil {
		t.Fatalf("GET /v1/...: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
		t.Errorf("API CSP = %q, want the strict API policy (web CSP must not leak into /v1)", got)
	}
}

// TestLandingRenders asserts the root renders the on-brand landing, not chi's
// bare 404 (the shell owns the web root).
func TestLandingRenders(t *testing.T) {
	s := newWebServer(t)
	rec := httptest.NewRecorder()
	s.handleLanding(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("landing status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AI-native artifact sharing") {
		t.Error("landing should render the on-brand tagline")
	}
}

// renderBinHTML renders the named Bin template ("bin" or "bin-rows") against a
// view model, exercising the template + row projection without a store.
func renderBinHTML(t *testing.T, s *Server, name string, vm binPageView) string {
	t.Helper()
	var sb strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&sb, name, vm); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return sb.String()
}

// TestBinRowRendersDesignFacts asserts a Bin row carries the full design-6c
// composition (SPEC-0001 REQ "The Bin Listing", design turn 6c): type badge +
// title + subtitle + provenance (`lead · channel · age`) + the three separate
// engagement counts (💬/🔥/🎯, never summed — SPEC-0006) + the TTL chip, plus
// the data-* attributes the client-side tab/filter lenses key on.
func TestBinRowRendersDesignFacts(t *testing.T) {
	s := newWebServer(t)
	vm := binPageView{
		Actor: "joe",
		Rows: []binRow{{
			ID: "abc123", Badge: "TRC", TypeLabel: "trajectory", TypeName: "trace",
			Title: "checkout-web-audit", Subtitle: "trajectory", WebURL: "/run/abc123",
			Lead: "claude", LeadShort: "claude", Channel: "via MCP", Age: "2h ago", TTL: "in 6d",
			ReactionCount: 3, CommentCount: 2, PinCount: 1,
			IsAgent: true, IsShared: true,
		}},
	}
	html := renderBinHTML(t, s, "bin", vm)
	for _, frag := range []string{
		"checkout-web-audit",          // title
		`class="bin-row-sub"`,         // subtitle slot
		"claude", "via MCP", "2h ago", // provenance line parts
		"💬", "🔥", "🎯", // the three engagement icons
		" comments", " reactions", " image region pins", // sr-only text cues (not color-only)
		`title="image region pins"`,             // the 🎯 glyph is self-explanatory (#72)
		`title="live — tails in real time"`,     // the live dot explains itself on hover (#72)
		`title="comments"`, `title="reactions"`, // the counter glyphs are labelled
		"in 6d",                             // TTL chip
		`title="expires in 6d"`,             // the TTL glyph is labelled too
		`data-agent="1"`, `data-shared="1"`, // visibility-lens + provenance flags
		`data-type-name="trace"`, // the type menu's reader-facing option label
		`data-type="trajectory"`, // live-dot / badge data hook (raw key, unchanged)
		// The reader-facing name is "trace" (ADR-0016), shown in the badge
		// tooltip and sr-only cue while data-type keeps the raw registry key.
		`>TRC<`, `title="trace"`, `<span class="sr-only"> trace</span>`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("bin row missing %q", frag)
		}
	}
}

// TestBinChromeRendersTabsFilterPush asserts the Bin toolbar exposes the three
// visibility tabs, the type menu, the filter input, and the `+ push` affordance
// with accessible labelling (design turn 6c; SPEC-0001 Accessibility).
func TestBinChromeRendersTabsFilterPush(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{Actor: "joe"})
	for _, frag := range []string{
		`role="tablist"`,
		`data-scope="all"`, `data-scope="shared"`, `data-scope="private"`,
		// Each tab names a visibility that actually partitions the bin and
		// carries a live-count slot (#67, #136): shared and private are
		// complements, so the counts always sum to All.
		">All <span", ">Shared <span", ">Private <span",
		`data-tab-count`,
		// The type menu is built client-side from the loaded rows, so the
		// server ships the shell and the row-level data hook only.
		`data-bin-types`, `data-bin-types-list`,
		`data-bin-filter`,
		`aria-label="Filter artifacts by title or provenance"`,
		"how to push",
		`aria-label="How to push an artifact"`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("bin chrome missing %q", frag)
		}
	}
}

// TestBinEmptyStateRenders proves the empty Bin shows an explicit empty state,
// not a spurious error or blank listing (SPEC-0001 REQ "Bin Empty State").
func TestBinEmptyStateRenders(t *testing.T) {
	s := newWebServer(t)
	html := renderBinHTML(t, s, "bin", binPageView{Actor: "joe"})
	if !strings.Contains(html, "Your bin is empty.") {
		t.Error("empty Bin should render the empty state")
	}
	if strings.Contains(html, `class="bin-row"`) {
		t.Error("empty Bin should render no rows")
	}
}

// TestBinRowsPartialCarriesLoadMore proves the HTMX "load more" partial appends
// a keyset-cursor button so pagination is a partial swap, not a full reload
// (SPEC-0001 REQ "Bin Pagination").
func TestBinRowsPartialCarriesLoadMore(t *testing.T) {
	s := newWebServer(t)
	vm := binPageView{
		NextCursor: "cursor-token",
		Rows:       []binRow{{ID: "abc123", Title: "a", TypeLabel: "markdown", Badge: "MD"}},
	}
	html := renderBinHTML(t, s, "bin-rows", vm)
	if !strings.Contains(html, `hx-get="/bin?cursor=cursor-token"`) {
		t.Error("load-more should carry the keyset cursor as an HTMX get")
	}
	if !strings.Contains(html, `hx-swap="outerHTML"`) {
		t.Error("load-more should swap itself out for the next page")
	}
}

func TestHumanizeHelpers(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now"}, // never "just now ago"
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeSince(now.Add(-c.in)); got != c.want {
			t.Errorf("humanizeSince(-%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := humanizeBytes(1234); got != "1.2 kB" {
		t.Errorf("humanizeBytes(1234) = %q, want 1.2 kB", got)
	}
	if got := humanizeBytes(512); got != "512 B" {
		t.Errorf("humanizeBytes(512) = %q, want 512 B", got)
	}

	// humanizeUntil frames the same magnitudes in the future tense. The
	// sub-minute case is the one humanizeDuration used to hand back as the
	// past-tense "just now", rendering the nonsense "in just now".
	untilCases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "in <1m"},
		{5 * time.Minute, "in 5m"},
		{50 * time.Hour, "in 2d"},
	}
	for _, c := range untilCases {
		// Add a beat so truncation does not drop the value into the bucket
		// below while the test is running.
		if got := humanizeUntil(now.Add(c.in + time.Second)); got != c.want {
			t.Errorf("humanizeUntil(+%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := humanizeUntil(now.Add(-time.Hour)); got != "expired" {
		t.Errorf("humanizeUntil(past) = %q, want expired", got)
	}
}

func TestTruncateAgentVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"crush/v0.66.2-0.20260724213111-ccae1f266157", "crush/v0.66.2"},
		{"claude-code/2.1.219", "claude-code/2.1.219"},
		{"claude", "claude"},
		{"", ""},
	}
	for _, c := range cases {
		if got := truncateAgentVersion(c.in); got != c.want {
			t.Errorf("truncateAgentVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatSecondsHumanizesLongDurations(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{2900, "2.9s"},
		{34164, "34.2s"},
		{45_000, "45.0s"},
		{89_999, "90.0s"},
		{90_000, "1m 30s"},
		{260_000, "4m 20s"},
		{3_600_000, "1h 0m"},
		{5_540_000, "1h 32m"},
		{7_200_000, "2h 0m"},
	}
	for _, c := range cases {
		if got := formatSeconds(c.ms); got != c.want {
			t.Errorf("formatSeconds(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// TestBinRowTruncatesPseudoVersion asserts a Bin row renders the truncated
// agent version with a tooltip carrying the full pseudo-version string.
func TestBinRowTruncatesPseudoVersion(t *testing.T) {
	s := newWebServer(t)
	vm := binPageView{
		Actor: "joe",
		Rows: []binRow{{
			ID: "abc123", Badge: "MD", TypeLabel: "markdown",
			Title: "report", WebURL: "/a/abc123",
			Lead:      "crush/v0.66.2-0.20260724213111-ccae1f266157",
			LeadShort: "crush/v0.66.2",
			Channel:   "via MCP", Age: "23h ago",
		}},
	}
	html := renderBinHTML(t, s, "bin", vm)
	if !strings.Contains(html, `title="crush/v0.66.2-0.20260724213111-ccae1f266157"`) {
		t.Error("bin row should carry full pseudo-version in tooltip")
	}
	if !strings.Contains(html, ">crush/v0.66.2<") {
		t.Error("bin row should render truncated pseudo-version")
	}
	if strings.Contains(html, "ccae1f266157") && !strings.Contains(html, `title="`) {
		t.Error("pseudo-version suffix should not appear in display text")
	}
}

func TestHumanizeDurationMS(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{90_000, "1m 30s"},
		{260_000, "4m 20s"},
		{3_600_000, "1h 0m"},
		{5_540_000, "1h 32m"},
		{7_200_000, "2h 0m"},
		{7_260_000, "2h 1m"},
	}
	for _, c := range cases {
		if got := humanizeDurationMS(c.ms); got != c.want {
			t.Errorf("humanizeDurationMS(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestFormatTokensZeroIsAbsent(t *testing.T) {
	// formatTokens still renders the number; the absent-value dash is handled
	// by the caller (buildTrajectoryView) which substitutes "—" with a tooltip.
	// This test proves the underlying formatter doesn't fabricate a zero.
	if got := formatTokens(0); got != "0" {
		t.Errorf("formatTokens(0) = %q, want \"0\"", got)
	}
	if got := formatTokens(48100); got != "48.1k" {
		t.Errorf("formatTokens(48100) = %q, want \"48.1k\"", got)
	}
	if got := formatTokens(900); got != "900" {
		t.Errorf("formatTokens(900) = %q, want \"900\"", got)
	}
}

// The asset directory is embedded wholesale (`//go:embed web/assets`) and
// served by a FileServer at /assets/*, so anything dropped in it ships inside
// the production binary and is publicly fetchable. The JS unit tests therefore
// live in web/jstests/, outside the embed root — this pins that boundary, so a
// future *_test.js placed beside the code it tests fails here instead of
// quietly becoming a public endpoint.
func TestAssetsShipNoTestFiles(t *testing.T) {
	err := fs.WalkDir(webAssetFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, "_test.js") || strings.Contains(filepath.Base(p), "_test.") {
			t.Errorf("test file %s is embedded in the asset FS and would be served at /assets/ — "+
				"move it to internal/httpapi/web/jstests/", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded asset FS: %v", err)
	}
}

// A bodyless artifact's meta line is rendered as visible text — the shell's
// artifact-meta span and the bin row subtitle both show it — so it must carry
// the registry DISPLAY NAME, never the raw share_type key. A trace's key stays
// "trajectory" until ADR-0016 Phase 2, and reading it here printed the retired
// word onto the page in two places at once.
//
// Governing: ADR-0016 (traces, not trajectories) Phase 1.
func TestMetaLineUsesDisplayNameNotRegistryKey(t *testing.T) {
	s := newWebServer(t)
	a := fixtureArtifact(artifact.TypeTrajectory, false)
	a.BodySHA256 = "" // bodyless: the branch that names the type

	if got := s.metaLine(a); got != "trace" {
		t.Errorf("metaLine for a bodyless trace = %q, want %q", got, "trace")
	}
	if strings.Contains(s.metaLine(a), "trajectory") {
		t.Error("the meta line must not surface the trajectory registry key as reader-facing text")
	}

	// A type whose key already reads well is unaffected — the display name
	// falls back to the key, so this is not a special case for one type.
	b := fixtureArtifact(sharetype.KeyWebhook, false)
	b.BodySHA256 = ""
	if got := s.metaLine(b); got != string(sharetype.KeyWebhook) {
		t.Errorf("metaLine for a bodyless webhook = %q, want its key", got)
	}
}
