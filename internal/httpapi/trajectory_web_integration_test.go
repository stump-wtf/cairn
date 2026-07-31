package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// readBody reads and closes a response body, returning it as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// auditSpans is the design's canonical checkout-web-audit run (trajectory-viewer
// -spec.md SPAN DATASET): a reasoning turn, a bash exec whose oversized output
// spills to a blob, a small inline read, a reasoning turn, a grep, a sub-agent
// excursion nesting web_search/web_fetch/reason children, a read, a compose
// turn, a write span producing a markdown artifact, and a final reasoning turn.
// Offsets are the design's left-percentages × the ~34.2s wall; durations are its
// second labels. Ingested as one closed batch, it is the acceptance fixture the
// waterfall renders faithfully.
func auditSpans(producedID string) []spanRequest {
	arg := func(k, v string) json.RawMessage { return json.RawMessage(`{"` + k + `":"` + v + `"}`) }
	return []spanRequest{
		{SpanID: "s1", Category: "reason", Name: "planned the audit", StartOffsetMS: 0, DurationMS: 2200},
		{SpanID: "s2", Category: "exec", Tool: "bash", Name: "npm ls --all", Args: arg("command", "npm ls --all"), Output: bigSpanOutput(), StartOffsetMS: 2223, DurationMS: 2900},
		{SpanID: "s3", Category: "read", Tool: "read", Name: "package.json", Args: arg("path", "package.json"), Output: []byte("{ \"name\": \"checkout-web\" }"), StartOffsetMS: 5130, DurationMS: 1400},
		{SpanID: "s4", Category: "reason", Name: "reviewed dependency tree", StartOffsetMS: 6498, DurationMS: 2700},
		{SpanID: "s5", Category: "exec", Tool: "grep", Name: "grep -r checkout", Args: arg("pattern", "checkout"), Output: []byte("src/checkout.ts:42\nsrc/pay.ts:19"), StartOffsetMS: 9234, DurationMS: 1400},
		{SpanID: "s6", Category: "net", Tool: "sub-agent", Name: "advisory lookup", StartOffsetMS: 10602, DurationMS: 10600},
		{SpanID: "s6a", ParentSpanID: "s6", Category: "net", Tool: "web_search", Name: "CVE search", Args: arg("query", "checkout CVE"), Output: []byte("3 advisories"), StartOffsetMS: 10944, DurationMS: 3800},
		{SpanID: "s6b", ParentSpanID: "s6", Category: "net", Tool: "web_fetch", Name: "fetch advisory", Args: arg("url", "https://cve.example/CVE-1"), Output: []byte("CVSS 9.1 · critical."), StartOffsetMS: 14877, DurationMS: 3900},
		{SpanID: "s6c", ParentSpanID: "s6", Category: "reason", Name: "weighed severity", StartOffsetMS: 18981, DurationMS: 2200},
		{SpanID: "s7", Category: "read", Tool: "read", Name: "package-lock.json", Args: arg("path", "package-lock.json"), Output: []byte("resolved 0.9.4"), StartOffsetMS: 21375, DurationMS: 1200},
		{SpanID: "s8", Category: "reason", Name: "composed the report", StartOffsetMS: 22572, DurationMS: 6100},
		{SpanID: "s9", Category: "write", Tool: "write", Name: "checkout-web-audit.md", Args: arg("path", "checkout-web-audit.md"), ProducedArtifactID: producedID, StartOffsetMS: 28728, DurationMS: 2700},
		{SpanID: "s10", Category: "reason", Name: "final summary", StartOffsetMS: 31464, DurationMS: 2700},
	}
}

// seedAuditRun creates the produced markdown artifact and ingests the canonical
// closed batch run as `joe`, returning the run id.
func seedAuditRun(t *testing.T, srvURL string) string {
	t.Helper()
	produced := createArtifact(t, srvURL, "markdown", "joe", "# Checkout Web Audit")
	resp := do(t, http.MethodPost, srvURL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "checkout-web-audit", Prompt: "Audit the checkout web app for known CVEs.",
			Model: "claude-sonnet-4.6", TokenCount: 48100, StartedAt: fixedRunStart, Spans: auditSpans(produced)}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed run = %d, want 201", resp.StatusCode)
	}
	return decodeRun(t, resp).ID
}

// TestIntegrationTrajectoryViewerRendersAuditRun is the MVP-finale acceptance:
// the canonical checkout-web-audit run renders faithfully at /run/{id} — the
// pinned span waterfall (every span row, category colors keyed on data-cat,
// sub-agent depth nesting, the time ruler), the activity stream (role markers,
// collapsible rows, tool chips, the write→artifact cross-link with its SHARED
// badge), and the RUN panel (provenance, the 2×2 derived stats, the
// time-by-category bar). Stats are DERIVED from the same span rows the waterfall
// draws, so the panel and the waterfall can never disagree (SPEC-0004).
func TestIntegrationTrajectoryViewerRendersAuditRun(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	runID := seedAuditRun(t, srv.URL)

	status, html := getHTML(t, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", runID, status)
	}

	// Header + shell chrome.
	for _, frag := range []string{
		`>RUN<`,
		`data-type="trajectory"`, // the registry key stays the data hook (#68)
		`checkout-web-audit`,
		`13 spans · 34.2s`,
		`mcp://cairn/run/` + runID,
		`aria-label="Span waterfall"`,
		`id="stream"`,
		`aria-label="Run details and comments"`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("audit viewer missing header/shell fragment %q", frag)
		}
	}

	// Waterfall: every span is a jump-target row with an accessible label, and
	// the category is exposed as data-cat so the JS live-append path and the CSS
	// share one palette.
	for _, span := range auditSpans("") {
		if !strings.Contains(html, `data-spanjump="`+span.SpanID+`"`) {
			t.Errorf("waterfall missing span row %q", span.SpanID)
		}
	}
	for _, frag := range []string{
		`data-cat="reason"`, `data-cat="exec"`, `data-cat="read"`, `data-cat="net"`, `data-cat="write"`,
		`data-tool="bash"`, `data-tool="grep"`, `data-tool="web_search"`, `data-tool="web_fetch"`,
		`data-tool="write"`, `data-tool="sub-agent"`,
		`wf-child`, // sub-agent children indent
		`└`,        // branch glyph
		// The ruler measures DURATION against the run's longest span (the 10.6s
		// sub-agent), not elapsed position: bars are sized that way, so an axis
		// labelled with wall-clock quartiles would misdescribe them. Quartiles
		// of 10.6s.
		`>0.0s<`, `>2.7s<`, `>5.3s<`, `>8.0s<`, `>10.6s<`,
		// Every bar carries its duration percentage, and the overview strip
		// carries the "when" the bars gave up.
		`data-dur-pct=`, `data-wf-timeline`, `class="tl-tick"`,
		// The strip's viewport box is also the scrubber (trajectory.js
		// wireTimelineDrag hangs off data-tl-viewport), and the hint tells the
		// reader it is grabbable.
		`data-tl-viewport`, `grab the timeline box to scrub`,
		// The scrubber is keyboard-operable (issue #61): a slider role with a
		// focus target and an accessible value range over the run's collapsed
		// seconds, not an aria-hidden span. A run whose rows all fit the pane
		// has no other pan control, so pointer-only locked keyboard users out.
		`role="slider"`, `tabindex="0"`, `aria-valuemin="0"`, `aria-valuemax=`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("waterfall missing fragment %q", frag)
		}
	}

	// Activity stream: the write→artifact cross-link opens the produced markdown
	// share and carries the SHARED badge (SPEC-0004 Produced-Artifact Link).
	if !strings.Contains(html, `class="badge badge-shared">SHARED<`) {
		t.Error("write row should carry the SHARED badge")
	}
	if !strings.Contains(html, `class="artifact-card"`) {
		t.Error("write row should render the produced-artifact cross-link card")
	}

	// The bash exec's oversized output spilled, so it loads lazily on expand
	// rather than inlining 20 KiB (SPEC-0004 lazy output).
	if !strings.Contains(html, `data-output-url="/v1/runs/`+runID+`/spans/s2/output"`) {
		t.Error("spilled span output should be lazily fetchable")
	}

	// RUN panel: derived 2×2 stats, provenance, time-by-category legend.
	for _, frag := range []string{
		`>34.2<`,  // wall seconds tile
		`>13<`,    // span count tile
		`>8<`,     // tool-call count tile
		`>48.1k<`, // tokens tile
		`captured`,
		`claude-sonnet-4.6`,
		`Time by category`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("RUN panel missing fragment %q", frag)
		}
	}

	// A closed run is not live.
	if strings.Contains(html, `data-live-badge`) {
		t.Error("a closed run must not show the live badge")
	}
	if !strings.Contains(html, `data-live="0"`) {
		t.Error("a closed run's waterfall should mark data-live=0")
	}
}

// TestIntegrationTrajectoryViewerRendersOpenCategorySet is the viewer half of
// issue #5: the category set is open (ADR-0009), so the waterfall and the RUN
// panel must render categories nobody designed a color for, and must derive the
// legend from the run rather than from a fixed list.
//
// The neutral color itself lives in CSS (`[data-cat] { --cat: var(--cat-other) }`
// resolves anything unrecognised), so what is asserted here is the contract the
// CSS depends on: the category reaches the markup as `data-cat`, its name is
// present as text so the color is never the only signal, and the legend covers
// exactly the categories this run used.
//
// Governing: ADR-0009 (open category set), SPEC-0004 REQ "Non-recommended
// category accepted", "Waterfall legend covers the categories a run used"
func TestIntegrationTrajectoryViewerRendersOpenCategorySet(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// `search` is recommended-but-new (added by PR #2); `deploy` and `vibes` are
	// outside the recommended set entirely — the neutral-color path.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "open-categories", Prompt: "Ship it.",
			Model: "claude-sonnet-4.6", StartedAt: fixedRunStart, Spans: []spanRequest{
				{SpanID: "a", Category: "reason", Name: "thought about it", StartOffsetMS: 0, DurationMS: 1000},
				{SpanID: "b", Category: "search", Tool: "grep", Name: "looked for callers", StartOffsetMS: 1000, DurationMS: 2000},
				{SpanID: "c", Category: "deploy", Tool: "kubectl", Name: "rolled it out", StartOffsetMS: 3000, DurationMS: 3000},
				{SpanID: "d", Category: "vibes", Name: "felt good about it", StartOffsetMS: 6000, DurationMS: 4000},
			}}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed open-category run = %d, want 201 (the category set is open)", resp.StatusCode)
	}
	runID := decodeRun(t, resp).ID

	status, html := getHTML(t, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", runID, status)
	}

	for _, cat := range []string{"reason", "search", "deploy", "vibes"} {
		if !strings.Contains(html, `data-cat="`+cat+`"`) {
			t.Errorf("viewer never exposes data-cat=%q, so the CSS cannot color it at all", cat)
		}
		// The legend entry carries the name as text (WCAG 1.4.1) — without it an
		// unrecognised category is a neutral bar with nothing explaining it.
		if !strings.Contains(html, `<li data-cat="`+cat+`"><span class="swatch" aria-hidden="true"></span>`+cat+`</li>`) {
			t.Errorf("waterfall legend has no text-labelled entry for %q", cat)
		}
	}

	// The legend is derived, not fixed: categories this run never used must not
	// appear in it, or it advertises colors nothing on the page carries.
	for _, unused := range []string{"exec", "read", "net", "write"} {
		if strings.Contains(html, `<li data-cat="`+unused+`">`) {
			t.Errorf("legend lists %q, a category this run never used", unused)
		}
	}

	// A tool on a non-recommended category is accepted and rendered — with an
	// open set only `reason` refuses one (ADR-0009).
	if !strings.Contains(html, `data-tool="kubectl"`) {
		t.Error("a tool on an unrecommended category should still render its chip")
	}

	// Time-by-category sums over every category, including the invented ones, or
	// the breakdown silently disagrees with the waterfall it is derived from.
	for _, frag := range []string{`Time by category`, `data-cat="deploy"`, `data-cat="vibes"`} {
		if !strings.Contains(html, frag) {
			t.Errorf("time-by-category breakdown missing %q", frag)
		}
	}

	// Span `d` is toolless and category `vibes`. The activity stream lays a
	// toolless span out as a reasoning turn, which under the CLOSED set was the
	// same statement as "its category is reason" — it no longer is. The chip must
	// name the span's own category, or one span is simultaneously labelled
	// "reason" in the stream and "vibes" in the legend it colour-matches.
	if !strings.Contains(html, `<span class="chip chip-cat" data-cat="vibes">`) {
		t.Error("a toolless non-reason span must chip its own category, not the reason chip")
	}
	if strings.Count(html, `<span class="chip chip-reason">reason</span>`) != 1 {
		t.Errorf("chip-reason should appear exactly once — for span `a`, the only genuine reason span; got %d",
			strings.Count(html, `<span class="chip chip-reason">reason</span>`))
	}
}

// TestIntegrationTrajectoryProvenanceLineReadsCleanly pins the PROVENANCE
// panel's captured line against two doubled-word regressions: the "via" prefix
// belongs to the artifact.Channel value ("via API"), never to the template, and
// the " ago" suffix belongs to humanizeSince, which suppresses it for the
// already-past-tense "just now". A freshly ingested run therefore reads
// "via API · captured just now".
func TestIntegrationTrajectoryProvenanceLineReadsCleanly(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	runID := seedAuditRun(t, srv.URL)

	_, html := getHTML(t, srv.URL+"/run/"+runID)
	if !strings.Contains(html, "via API · captured just now") {
		t.Error("provenance line should read `via API · captured just now`")
	}
	for _, bad := range []string{"via via", "just now ago"} {
		if strings.Contains(html, bad) {
			t.Errorf("provenance line should not contain %q", bad)
		}
	}
}

// TestIntegrationTrajectoryReactionsRender asserts reactions on a turn
// (trajectory_turn) and a tool call (trajectory_toolcall) — the SPEC-0006
// trajectory reaction anchors — render as pills with their counts on the
// matching span rows after being posted through the annotation core.
func TestIntegrationTrajectoryReactionsRender(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	runID := seedAuditRun(t, srv.URL)

	// React on the opening reasoning turn s1 and the bash tool call s2.
	react := func(anchorType, spanID, emoji string) {
		resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+runID+"/reactions", "alice",
			jsonReader(t, reactionRequest{AnchorType: anchorType, AnchorRef: json.RawMessage(`{"span_id":"` + spanID + `"}`), Emoji: emoji}),
			"application/json")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("react %s/%s = %d, want 201", anchorType, spanID, resp.StatusCode)
		}
		resp.Body.Close()
	}
	react("trajectory_turn", "s1", "🎯")
	react("trajectory_toolcall", "s2", "👀")

	_, html := getHTML(t, srv.URL+"/run/"+runID)
	if !strings.Contains(html, `data-emoji="🎯"`) {
		t.Error("turn reaction pill should render on the run")
	}
	if !strings.Contains(html, `data-emoji="👀"`) {
		t.Error("tool-call reaction pill should render on the run")
	}
	if !strings.Contains(html, `data-react-cluster`) {
		t.Error("reaction clusters should be present for the pickers to attach to")
	}
}

// TestIntegrationTrajectorySpanCommentRendersContext asserts a comment anchored
// to a trajectory_span, posted through the shell's web comment route with a
// session, renders in the panel with its purple anchor-context prefix (design
// t7a "on span"). It exercises the extended web comment anchoring (span_id form
// field) end-to-end under session auth + CSRF.
func TestIntegrationTrajectorySpanCommentRendersContext(t *testing.T) {
	srv, client := sessionServer(t)
	runID := seedAuditRun(t, srv.URL)

	resp := doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	if csrf == "" {
		t.Fatal("login did not seed a CSRF cookie")
	}

	// Comment on span s6 (the sub-agent) via the web composer, anchored by the
	// span_id form field the trajectory viewer sets.
	resp = postForm(t, client, srv.URL+"/"+runID+"/comments",
		url.Values{"body": {"nice CVE catch"}, "span_id": {"s6"}}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web span comment = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "nice CVE catch") || !strings.Contains(body, "on span s6") {
		t.Errorf("comment partial should carry the body and the anchor context, got %q", body)
	}

	// It also renders in a fresh page load of the panel thread.
	_, html := getHTML(t, srv.URL+"/run/"+runID)
	if !strings.Contains(html, "on span s6") {
		t.Error("panel should render the span comment's anchor context")
	}
}

// TestIntegrationTrajectoryLiveRunStreams asserts an open (live) run marks
// itself live in the viewer and that its SSE span stream replays the captured
// spans then reports the run's status — the data path trajectory.js drives to
// append live span rows (SPEC-0004 Live Span Stream Delivery).
func TestIntegrationTrajectoryLiveRunStreams(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// Open a live run seeded with the first two spans.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "checkout-web-audit", Prompt: "Audit.",
			Model: "claude-sonnet-4.6", StartedAt: fixedRunStart, Spans: auditSpans("")[:2]}),
		"application/json")
	run := decodeRun(t, resp)

	_, html := getHTML(t, srv.URL+"/run/"+run.ID)
	if !strings.Contains(html, `data-live-badge`) || !strings.Contains(html, `data-live="1"`) {
		t.Error("an open run should render the live badge and mark the waterfall live")
	}
	if !strings.Contains(html, `data-live-region`) {
		t.Error("the stream should carry an aria-live region for streaming announcements")
	}

	// The SSE endpoint replays the captured spans and the status.
	streamResp := do(t, http.MethodGet, srv.URL+"/v1/runs/"+run.ID+"/stream?last_event_id=0", "", nil, "")
	defer streamResp.Body.Close()
	if ct := streamResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}
	buf := make([]byte, 4096)
	n, _ := streamResp.Body.Read(buf)
	frame := string(buf[:n])
	if !strings.Contains(frame, "event: span") {
		t.Errorf("stream should replay a span event, got %q", frame)
	}
}

// TestIntegrationTrajectoryRendersArgsAndEmptySpans is the regression fixture
// for the run that exposed all of this: a client captured twelve spans using
// SDLC-phase categories, put its structured context in `args`, and sent no
// `output` on any span. The viewer rendered a uniformly neutral waterfall of
// rows that expanded onto blank space — the args were ingested, stored, and
// returned by the API, then silently dropped by the template.
//
// This pins all three halves of the fix at once, because they only fail
// together in a real render: the phase vocabulary is recognised (so the
// stylesheet, not the JS hash fallback, colors it), `args` reach the page, and
// a span with genuinely nothing to show says so instead of opening onto
// nothing.
//
// Governing: SPEC-0004 REQ "Activity Stream" (args and output are shown),
// ADR-0009 (open category set)
func TestIntegrationTrajectoryRendersArgsAndEmptySpans(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "msgbrowse #227", Prompt: "Add a status-banner component.",
			Model: "glm-5.2", TokenCount: 45000, StartedAt: fixedRunStart, Spans: []spanRequest{
				// Carries args, no output: args must render, and the row is NOT empty.
				{SpanID: "p1", Category: "research", Name: "Load issue context",
					Args:          json.RawMessage(`{"repo":"stump.wtf/msgbrowse","issue":"227"}`),
					StartOffsetMS: 0, DurationMS: 120000},
				// The `{}` placeholder agents habitually send: renders as nothing,
				// so this row IS empty and must say so.
				{SpanID: "p2", Category: "implementation", Name: "Add status_banner partial",
					Args: json.RawMessage(`{}`), StartOffsetMS: 120000, DurationMS: 60000},
				// Neither args nor output at all — the plainest empty row.
				{SpanID: "p3", Category: "delivery", Name: "Open PR #229",
					StartOffsetMS: 180000, DurationMS: 30000},
				// A span WITH output must not get the empty state.
				{SpanID: "p4", Category: "testing", Tool: "bash", Name: "go test ./...",
					Args:   json.RawMessage(`{"command":"go test ./..."}`),
					Output: []byte("ok  	msgbrowse	0.4s"), StartOffsetMS: 210000, DurationMS: 90000},
			}}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed phase run = %d, want 201", resp.StatusCode)
	}
	runID := decodeRun(t, resp).ID

	status, html := getHTML(t, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", runID, status)
	}

	// The phase vocabulary reaches the page as real categories, so trajectory.css
	// colors them and the JS hash fallback never engages.
	for _, cat := range []string{"research", "implementation", "delivery", "testing"} {
		if !strings.Contains(html, `data-cat="`+cat+`"`) {
			t.Errorf("phase category %q missing from the render", cat)
		}
	}

	// args render, pretty-printed and HTML-escaped into the <pre>.
	for _, frag := range []string{
		`class="args-pre"`,
		`&#34;repo&#34;: &#34;stump.wtf/msgbrowse&#34;`,
		`&#34;issue&#34;: &#34;227&#34;`,
		`&#34;command&#34;: &#34;go test ./...&#34;`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("args fragment %q missing from the render", frag)
		}
	}

	// An empty `{}` contributes no args block of its own.
	if strings.Contains(html, `<pre class="args-pre">{}</pre>`) {
		t.Error("an empty args object should render nothing, not an empty block")
	}

	// Exactly the two genuinely-empty spans (p2, p3) carry the empty state;
	// p1 has args and p4 has output, so neither may.
	if got := strings.Count(html, `class="detail-empty"`); got != 2 {
		t.Errorf("empty-state count = %d, want 2 (only the spans with no args and no output)", got)
	}
	if !strings.Contains(html, "No output captured for this span") {
		t.Error("a span with nothing to reveal must explain itself, not expand onto blank space")
	}

	// The span that does have output still renders it, and is not called empty.
	if !strings.Contains(html, `ok  	msgbrowse	0.4s`) {
		t.Error("a span's output must still render")
	}
}

// TestIntegrationTrajectorySynthesizesOpeningPromptMarker pins the waterfall
// opening with the human's prompt. Most captures so far emitted no opening
// prompt span, so the graph opened mid-thought on the first tool call; the
// viewer now synthesizes a zero-length 💬 marker from the run's prompt field —
// wired to the stream's existing "__prompt__" turn so the jump lands on the
// real prompt card — and stands down when the capture already provides one.
func TestIntegrationTrajectorySynthesizesOpeningPromptMarker(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	seed := func(t *testing.T, spans []spanRequest) string {
		t.Helper()
		resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
			jsonReader(t, runRequest{Mode: "batch", Title: "opening marker", Prompt: "Fix the bugs in harness.",
				Model: "kimi-k3", StartedAt: fixedRunStart, Spans: spans}),
			"application/json")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed run = %d, want 201", resp.StatusCode)
		}
		return decodeRun(t, resp).ID
	}

	// No opening prompt span in the capture: the marker is synthesized.
	bare := seed(t, []spanRequest{
		{SpanID: "a", Category: "reason", Name: "Assistant turn", StartOffsetMS: 0, DurationMS: 560000},
		{SpanID: "b", Category: "tool", Tool: "view", Name: "view", StartOffsetMS: 560000, DurationMS: 7000},
	})
	_, html := getHTML(t, srv.URL+"/run/"+bare)
	if !strings.Contains(html, `data-spanjump="__prompt__"`) {
		t.Error("a run without an opening prompt span must get a synthesized 💬 marker row")
	}
	if !strings.Contains(html, "Fix the bugs in harness.") {
		t.Error("the synthesized marker should carry the prompt as its label")
	}

	// The capture already opens with a prompt span: no synthetic duplicate.
	explicit := seed(t, []spanRequest{
		{SpanID: "p0", Category: "prompt", Name: "Joe: fix the bugs", StartOffsetMS: 0, DurationMS: 0},
		{SpanID: "a", Category: "reason", Name: "Assistant turn", StartOffsetMS: 0, DurationMS: 560000},
	})
	_, html2 := getHTML(t, srv.URL+"/run/"+explicit)
	if strings.Contains(html2, `data-spanjump="__prompt__"`) {
		t.Error("a capture that already opens with a prompt span must not get a second, synthetic one")
	}
}

// TestIntegrationTrajectoryPromptRendersAsMarker pins the prompt treatment: a
// human taking minutes to type is real elapsed time but says nothing about how
// the run performed, and as a proportional bar it dwarfed every unit of actual
// work either side of it.
//
// So a `prompt` span keeps its place on the time axis and its full turn in the
// stream, but the waterfall draws it as a marker and the time-by-category
// breakdown leaves it out. A `wait` span — blocked on CI, a rate limit — is a
// genuine performance fact and keeps its bar and its slice.
func TestIntegrationTrajectoryPromptRendersAsMarker(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "prompt marker", Prompt: "Do some work.",
			Model: "claude-opus-5", StartedAt: fixedRunStart, Spans: []spanRequest{
				{SpanID: "a", Category: "research", Name: "Read the code", StartOffsetMS: 0, DurationMS: 20000},
				{SpanID: "b", Category: "prompt", Name: "Joe: now the other thing", StartOffsetMS: 20000, DurationMS: 540000},
				{SpanID: "c", Category: "wait", Name: "Blocked on CI", StartOffsetMS: 560000, DurationMS: 45000},
			}}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed run = %d, want 201", resp.StatusCode)
	}
	runID := decodeRun(t, resp).ID

	status, html := getHTML(t, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", runID, status)
	}

	// The span is still fully present: legend entry, waterfall row, stream turn.
	for _, frag := range []string{`data-cat="prompt"`, "Joe: now the other thing"} {
		if !strings.Contains(html, frag) {
			t.Errorf("a prompt span must still render (%q missing)", frag)
		}
	}

	// But it contributes no time-by-category segment, while wait still does.
	segs := strings.Count(html, `class="cat-seg"`)
	if strings.Contains(html, `<span class="cat-seg" data-cat="prompt"`) {
		t.Error("prompt must not take a slice of the time-by-category bar — it is elapsed time, not effort")
	}
	if !strings.Contains(html, `data-cat="wait"`) {
		t.Error("wait is a real performance fact and must keep its place")
	}
	if segs == 0 {
		t.Error("expected the breakdown to still render segments for the other categories")
	}
}

// TestIntegrationTrajectoryCategoryBarIsComposition pins the TIME BY CATEGORY
// bar's denominator. It answers "of the time the agent spent working, where did
// it go?" — a composition that always fills the bar — not "what fraction of the
// wall clock was categorized?". Divided by wall time, a run that idled for
// hours around minutes of work rendered every segment under 1% and the panel
// looked broken (observed: 31h wall around 29min of work → a sliver).
func TestIntegrationTrajectoryCategoryBarIsComposition(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Title: "idle heavy", Prompt: "Work briefly, idle enormously.",
			Model: "kimi-k3", StartedAt: fixedRunStart, Spans: []spanRequest{
				{SpanID: "a", Category: "reason", Name: "think", StartOffsetMS: 0, DurationMS: 30_000},
				// A day of dead air the composition must ignore.
				{SpanID: "b", Category: "tool", Tool: "bash", Name: "run", StartOffsetMS: 86_430_000, DurationMS: 70_000},
			}}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed run = %d, want 201", resp.StatusCode)
	}
	runID := decodeRun(t, resp).ID

	_, html := getHTML(t, srv.URL+"/run/"+runID)
	// 30s of 100s categorized = 30%, 70s = 70% — regardless of the ~24h wall.
	for _, frag := range []string{`data-cat="reason" data-pct="30.0"`, `data-cat="tool" data-pct="70.0"`} {
		if !strings.Contains(html, frag) {
			t.Errorf("category bar missing %q — segments must be shares of categorized time, not wall time", frag)
		}
	}
}
