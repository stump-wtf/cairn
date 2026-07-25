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
		`>TRJ<`,
		`data-type="trajectory"`,
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
		`wf-child`,                     // sub-agent children indent
		`└`,                            // branch glyph
		`>8.5s<`, `>17.1s<`, `>34.2s<`, // ruler ticks derived from wall time
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
