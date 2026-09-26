// Integration coverage for the A2UI MCP resources (issue #90): all three
// surfaces (run, bundle, artifact), happy and unhappy paths. These run
// against the real
// streamable-HTTP MCP transport with a real OAuth-minted token over real
// Postgres — the same shape as the rest of the MCP integration suite, so
// every assertion exercises what an A2UI-capable host will actually speak.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/store"
)

// decodeA2UI is a small assertion helper: it reads the named resource,
// asserts it carries exactly one content part with the A2UI MIME type, and
// decodes the envelope. Failing any of those steps fails the test.
func decodeA2UI(t *testing.T, sess *mcp.ClientSession, uri string) a2uiEnvelope {
	t.Helper()
	res, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("%s contents = %+v, want exactly one part", uri, res.Contents)
	}
	if got := res.Contents[0].MIMEType; got != a2uiMIME {
		t.Fatalf("%s MIME = %q, want %q", uri, got, a2uiMIME)
	}
	var env a2uiEnvelope
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &env); err != nil {
		t.Fatalf("decode %s a2ui: %v\nbody: %s", uri, err, res.Contents[0].Text)
	}
	return env
}

// a2uiIndex builds an id -> component map for the envelope's components,
// making shape assertions read like lookups rather than linear scans.
func a2uiIndex(env a2uiEnvelope) map[string]a2uiComponent {
	out := make(map[string]a2uiComponent, len(env.UpdateComponents.Components))
	for _, c := range env.UpdateComponents.Components {
		id, _ := c["id"].(string)
		if id != "" {
			out[id] = c
		}
	}
	return out
}

// TestIntegrationMCPRunA2UI covers the trace view end-to-end: an open run
// with a seeded span tree renders as an A2UI envelope with a header card, a
// stats card (category bar + legend + hot spots), and a flame graph whose
// rows carry the span tree's depth (indented label), category, name,
// timeline bar and duration.
func TestIntegrationMCPRunA2UI(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "run_create", map[string]any{
		"title":  "triage the failing test",
		"prompt": "make CI green",
		"model":  "test-model",
		"spans": []map[string]any{
			{
				"span_id":         "root-1",
				"category":        "exec",
				"tool":            "bash",
				"name":            "go test ./...",
				"start_offset_ms": 0,
				"duration_ms":     500,
			},
			{
				"span_id":         "child-1",
				"parent_span_id":  "root-1",
				"category":        "read",
				"tool":            "read_file",
				"name":            "view the failure",
				"start_offset_ms": 100,
				"duration_ms":     50,
			},
		},
	})
	var created_run mcpRunOutput
	decodeToolJSON(t, created, &created_run)
	if created_run.ID == "" {
		t.Fatalf("run_create returned no id: %+v", created_run)
	}

	env := decodeA2UI(t, sess, "mcp://cairn/run/"+created_run.ID+"/a2ui")

	if env.Version != a2uiVersion {
		t.Fatalf("envelope version = %q, want %q", env.Version, a2uiVersion)
	}
	if env.UpdateComponents.CatalogID != a2uiCatalog {
		t.Fatalf("catalogId = %q, want %q", env.UpdateComponents.CatalogID, a2uiCatalog)
	}
	if !strings.HasSuffix(env.UpdateComponents.SurfaceID, created_run.ID) {
		t.Fatalf("surfaceId = %q, want it to carry the run id", env.UpdateComponents.SurfaceID)
	}

	idx := a2uiIndex(env)

	// Header Card carries the run's title.
	hdr, ok := idx["hdr"]
	if !ok || hdr["component"] != "Card" {
		t.Fatalf("no header card in components: %+v", idx)
	}
	title, ok := idx["hdr-title"]
	if !ok || title["text"] != "triage the failing test" {
		t.Fatalf("hdr-title = %+v, want the run's title", title)
	}

	// Stats row carries wall time, span count, tool call count and tokens.
	for _, id := range []string{"stats-wall", "stats-spans", "stats-tools", "stats-tokens"} {
		if _, ok := idx[id]; !ok {
			t.Fatalf("missing stats component %s", id)
		}
	}
	if got := idx["stats-spans"]["text"]; got != "2 spans" {
		t.Fatalf("stats-spans text = %v, want %q", got, "2 spans")
	}

	// The stats card carries the category story: a stacked share bar, a
	// legend naming both categories with the same glyphs the flame bars
	// use (exec dominates → densest glyph '█'), and the self-time hot
	// spots (the root's 500ms minus its child's 50ms ranks first). Glyph
	// swatches carry ANSI color, so substring checks run on the stripped
	// text.
	dist, ok := idx["stats-dist"]
	distText, _ := dist["text"].(string)
	if !ok || !strings.ContainsRune(distText, '█') {
		t.Fatalf("stats-dist = %q, want a stacked category bar", distText)
	}
	legend, ok := idx["stats-legend"]
	legendText, _ := legend["text"].(string)
	legendPlain := a2uiStripANSI(legendText)
	if !ok || !strings.Contains(legendPlain, "█ exec") || !strings.Contains(legendPlain, "read") {
		t.Fatalf("stats-legend = %q, want glyphed categories", legendText)
	}
	hot, ok := idx["stats-hot"]
	hotText, _ := hot["text"].(string)
	if !ok || !strings.Contains(hotText, "go test ./... 450ms") {
		t.Fatalf("stats-hot = %q, want the root span's 450ms self time first", hotText)
	}

	// The flame graph opens with a timeline ruler spanning the gutter.
	axis, ok := idx["flame-axis"]
	axisText, _ := axis["text"].(string)
	if !ok || !strings.Contains(axisText, "0┄") || !strings.Contains(axisText, "500ms") {
		t.Fatalf("flame-axis = %q, want a 0→wall ruler", axisText)
	}

	// Two flame rows; the child row is indented relative to the root row,
	// and both carry the span's category, name, a timeline bar in the
	// category's glyph, and the duration after the gutter.
	span1, ok1 := idx["span-1"]
	span2, ok2 := idx["span-2"]
	if !ok1 || !ok2 {
		t.Fatalf("missing span components: %+v", idx)
	}
	text1, _ := span1["text"].(string)
	text2, _ := span2["text"].(string)
	if !strings.Contains(text1, "[exec]") || !strings.Contains(text1, "go test ./...") {
		t.Fatalf("root span label = %q, want category + name", text1)
	}
	if !strings.Contains(text2, "[read]") || !strings.Contains(text2, "view the failure") {
		t.Fatalf("child span label = %q, want category + name", text2)
	}
	if !strings.HasPrefix(text2, "  ") {
		t.Fatalf("child span label = %q, want depth indentation", text2)
	}
	if strings.HasPrefix(text1, "  ") {
		t.Fatalf("root span label = %q, must NOT be indented", text1)
	}
	if !strings.ContainsRune(text1, '█') {
		t.Fatalf("root span row = %q, want a '█' bar (exec is the top category)", text1)
	}
	if !strings.ContainsRune(text2, '▓') {
		t.Fatalf("child span row = %q, want a '▓' bar (read ranks second)", text2)
	}
	if !strings.Contains(text1, "▕") || !strings.Contains(text1, "▏") {
		t.Fatalf("root span row = %q, want gutter edges", text1)
	}
	if !strings.Contains(text1, "500ms") || !strings.Contains(text2, "50ms") {
		t.Fatalf("span rows %q / %q, want durations after the gutter", text1, text2)
	}

	// Regression: Text components must use the spec-defined "variant" field
	// (not the invented "usageHint"). A2UI v0.9 catalog schemas set
	// unevaluatedProperties: false, so "usageHint" would be silently
	// dropped by a compliant validator, leaving headings and captions
	// unstyled. Assert on both the run and bundle title (h2) and the stats
	// caption row to cover both surfaces.
	if v, ok := idx["hdr-title"]["variant"]; !ok || v != "h2" {
		t.Fatalf("hdr-title variant = %v, want %q (must not be usageHint)", v, "h2")
	}
	if v, ok := idx["hdr-meta"]["variant"]; !ok || v != "caption" {
		t.Fatalf("hdr-meta variant = %v, want %q (must not be usageHint)", v, "caption")
	}
	if _, ok := idx["hdr-title"]["usageHint"]; ok {
		t.Fatal("hdr-title must NOT carry usageHint — the spec field is variant")
	}
}

// TestIntegrationMCPRunA2UIRejectsBadURI proves the unhappy path: a malformed
// a2ui URI is rejected with a validation failure rather than a confusing
// not-found, and a read against an unknown run id surfaces the store's
// uniform not-found.
func TestIntegrationMCPRunA2UIRejectsBadURI(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	// A bare run URI (no /a2ui suffix) routes to the JSON template's handler,
	// which rejects the unknown id "whatever" — so this read fails, but for
	// the JSON handler's reason, not the a2ui handler's. The a2ui-specific
	// unhappy path is the malformed URI below.
	_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/run/whatever",
	})
	if err == nil {
		t.Fatal("reading a bare run URI for an unknown id must fail")
	}

	// A malformed a2ui URI — the SDK matches the template but the {id}
	// segment is empty — is rejected by the a2ui handler's own validation.
	_, err = sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/run//a2ui",
	})
	if err == nil || !strings.Contains(err.Error(), "not a run a2ui resource URI") {
		t.Fatalf("empty run id a2ui URI error = %v, want a validation failure", err)
	}

	// An unknown run id surfaces a uniform not-found from the store.
	_, err = sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/run/unknownid999/a2ui",
	})
	if err == nil {
		t.Fatal("unknown run id must fail")
	}
}

// TestIntegrationMCPRunA2UIRequiresRead proves the resource honours the same
// single artifacts:read scope the JSON form requires — no fourth scope, and
// no scope-less reads.
func TestIntegrationMCPRunA2UIRequiresRead(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	runID := openRunViaMCP(t, srv, humanToken)

	// A write-only token must be refused — this is the scope-check unhappy
	// path, and matches what the JSON resource enforces.
	writeOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	writeSess := mcpClient(t, srv, writeOnly, nil, "writeonly-agent")
	_, err := writeSess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/run/" + runID + "/a2ui",
	})
	if err == nil {
		t.Fatal("write-only token must not read the a2ui run resource")
	}

	// A read-only token works.
	readOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	readSess := mcpClient(t, srv, readOnly, nil, "readonly-agent")
	if _, err := readSess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/run/" + runID + "/a2ui",
	}); err != nil {
		t.Fatalf("read-only token must read the a2ui run resource: %v", err)
	}
}

// TestIntegrationMCPRunA2UIEmptyRun covers the boundary case: a run with no
// spans renders an empty-state marker instead of crashing or fabricating a
// span row.
func TestIntegrationMCPRunA2UIEmptyRun(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	runID := openRunViaMCP(t, srv, token)

	sess := mcpClient(t, srv, token, nil, "a2ui-agent")
	env := decodeA2UI(t, sess, "mcp://cairn/run/"+runID+"/a2ui")
	idx := a2uiIndex(env)
	if _, ok := idx["span-empty"]; !ok {
		t.Fatalf("empty run must render the span-empty marker; got %+v", idx)
	}
}

// a2uiSpanBatch builds n flat root-level spans for run_create, enough to
// probe the a2uiMaxRunSpans cap.
func a2uiSpanBatch(n int) []map[string]any {
	spans := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		spans = append(spans, map[string]any{
			"span_id":         fmt.Sprintf("s%d", i),
			"category":        "exec",
			"name":            fmt.Sprintf("step %d", i),
			"start_offset_ms": i,
			"duration_ms":     1,
		})
	}
	return spans
}

// TestIntegrationMCPRunA2UISpanCap covers both sides of the a2uiMaxRunSpans
// boundary: a run holding exactly the cap renders every span and NO overflow
// row (the "…0 more spans" regression), while one span over the cap renders
// the cap plus an overflow row counting exactly the omitted spans.
func TestIntegrationMCPRunA2UISpanCap(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	// Exactly at the cap: all spans render, no bogus overflow row.
	created := callTool(t, sess, "run_create", map[string]any{
		"title": "exactly the cap",
		"spans": a2uiSpanBatch(a2uiMaxRunSpans),
	})
	var atCap mcpRunOutput
	decodeToolJSON(t, created, &atCap)

	env := decodeA2UI(t, sess, "mcp://cairn/run/"+atCap.ID+"/a2ui")
	idx := a2uiIndex(env)
	if _, ok := idx[fmt.Sprintf("span-%d", a2uiMaxRunSpans)]; !ok {
		t.Fatalf("exactly-at-cap run must render all %d spans", a2uiMaxRunSpans)
	}
	if _, ok := idx["span-overflow"]; ok {
		t.Fatalf("exactly-at-cap run must NOT render the overflow row: %v", idx["span-overflow"])
	}

	// One over the cap: the cap renders, plus an overflow row naming the
	// single omitted span.
	created = callTool(t, sess, "run_create", map[string]any{
		"title": "one over the cap",
		"spans": a2uiSpanBatch(a2uiMaxRunSpans + 1),
	})
	var overCap mcpRunOutput
	decodeToolJSON(t, created, &overCap)

	env = decodeA2UI(t, sess, "mcp://cairn/run/"+overCap.ID+"/a2ui")
	idx = a2uiIndex(env)
	overflow, ok := idx["span-overflow"]
	if !ok {
		t.Fatal("over-cap run must render the overflow row")
	}
	text, _ := overflow["text"].(string)
	if !strings.Contains(text, "…1 more spans") {
		t.Fatalf("overflow row = %q, want it to count exactly 1 omitted span", text)
	}
	if _, ok := idx[fmt.Sprintf("span-%d", a2uiMaxRunSpans+1)]; ok {
		t.Fatalf("over-cap run must stop rendering at %d spans", a2uiMaxRunSpans)
	}
}

// TestIntegrationMCPBundleA2UI covers the bundle view end-to-end: a bundle
// created via bundle_create renders as an envelope header Card (member
// count, total size, type mix) plus a member list row per member, each
// carrying the member's name and a size-bar + size + badge + media-type
// caption.
func TestIntegrationMCPBundleA2UI(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title": "the q3 audit",
		"members": []map[string]any{
			{"name": "README.md", "body": "# audit", "media_type": "text/markdown"},
			{"name": "main.go", "body": "package main", "media_type": "text/x-go"},
		},
	})
	var createdBundle mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdBundle)
	if createdBundle.ID == "" {
		t.Fatalf("bundle_create returned no id: %+v", createdBundle)
	}

	env := decodeA2UI(t, sess, "mcp://cairn/bundle/"+createdBundle.ID+"/a2ui")
	if env.Version != a2uiVersion {
		t.Fatalf("envelope version = %q, want %q", env.Version, a2uiVersion)
	}
	if !strings.HasSuffix(env.UpdateComponents.SurfaceID, createdBundle.ID) {
		t.Fatalf("surfaceId = %q, want it to carry the bundle id", env.UpdateComponents.SurfaceID)
	}

	idx := a2uiIndex(env)
	if _, ok := idx["hdr"]; !ok {
		t.Fatalf("no header card: %+v", idx)
	}
	if got := idx["hdr-title"]["text"]; got != "the q3 audit" {
		t.Fatalf("hdr-title = %v, want the bundle title", got)
	}

	// The envelope caption aggregates the members: count and total size.
	hdrMeta, ok := idx["hdr-meta"]
	hdrMetaText, _ := hdrMeta["text"].(string)
	if !ok || !strings.Contains(hdrMetaText, "2 members") || !strings.Contains(hdrMetaText, "total") {
		t.Fatalf("hdr-meta = %q, want member count and total size", hdrMetaText)
	}

	// The type-mix caption aggregates the rail's badges: one markdown
	// member (MD) and one Go source member (CODE via ClassifyMember).
	mix, ok := idx["hdr-mix"]
	mixText, _ := mix["text"].(string)
	if !ok || !strings.Contains(mixText, "1 MD") || !strings.Contains(mixText, "1 CODE") {
		t.Fatalf("hdr-mix = %q, want badge counts (1 MD · 1 CODE)", mixText)
	}

	// Two member rows, each carrying the member's name and a caption with a
	// relative-size bar, size, type badge and media type.
	m0Name, ok0 := idx["member-0-name"]
	m1Name, ok1 := idx["member-1-name"]
	if !ok0 || !ok1 {
		t.Fatalf("missing member name components: %+v", idx)
	}
	if m0Name["text"] != "README.md" || m1Name["text"] != "main.go" {
		t.Fatalf("member names = %v, %v; want README.md and main.go", m0Name["text"], m1Name["text"])
	}

	// Story #105: each member row is a Button carrying an open_member action
	// whose context is {bundle, member} raw, wrapping the same content column.
	// The members-list references the button ids, and every button child
	// resolves (no dangling refs).
	assertMemberButton := func(i int, wantName string) {
		t.Helper()
		btnID := fmt.Sprintf("member-%d-btn", i)
		btn, ok := idx[btnID]
		if !ok {
			t.Fatalf("missing %s: %+v", btnID, idx)
		}
		if got := btn["component"]; got != "Button" {
			t.Fatalf("%s component = %v, want Button", btnID, got)
		}
		if got := btn["child"]; got != fmt.Sprintf("member-%d-col", i) {
			t.Fatalf("%s child = %v, want member-%d-col", btnID, got, i)
		}
		action, _ := btn["action"].(map[string]any)
		event, _ := action["event"].(map[string]any)
		if got := event["name"]; got != "open_member" {
			t.Fatalf("%s action.event.name = %v, want open_member", btnID, got)
		}
		ctx, _ := event["context"].(map[string]any)
		if got := ctx["bundle"]; got != createdBundle.ID {
			t.Fatalf("%s context.bundle = %v, want %q", btnID, got, createdBundle.ID)
		}
		if got := ctx["member"]; got != wantName {
			t.Fatalf("%s context.member = %v, want %q", btnID, got, wantName)
		}
		// The child column must resolve to a real component (no dangling ref).
		if _, ok := idx[btn["child"].(string)]; !ok {
			t.Fatalf("%s child %v dangles (no such component)", btnID, btn["child"])
		}
	}
	assertMemberButton(0, "README.md")
	assertMemberButton(1, "main.go")
	if list, ok := idx["members-list"]; ok {
		raw, _ := list["children"].([]any)
		children := make([]string, 0, len(raw))
		for _, c := range raw {
			children = append(children, fmt.Sprintf("%v", c))
		}
		if len(children) != 2 || children[0] != "member-0-btn" || children[1] != "member-1-btn" {
			t.Fatalf("members-list children = %v, want [member-0-btn member-1-btn]", children)
		}
	} else {
		t.Fatal("missing members-list")
	}
	m0Meta, ok := idx["member-0-meta"]
	if !ok {
		t.Fatalf("missing member-0-meta: %+v", idx)
	}
	metaText, _ := m0Meta["text"].(string)
	if !strings.Contains(metaText, "text/markdown") {
		t.Fatalf("member-0-meta = %q, want the media type", metaText)
	}
	if !strings.Contains(metaText, "MD") {
		t.Fatalf("member-0-meta = %q, want the MD type badge", metaText)
	}
	if !strings.ContainsRune(metaText, '█') {
		t.Fatalf("member-0-meta = %q, want a relative-size bar", metaText)
	}

	// README.md ("# audit", 7 B) is smaller than main.go ("package main",
	// 12 B), so its bar is partly track ('·' run) while main.go's is full.
	if !strings.Contains(metaText, "··") {
		t.Fatalf("member-0-meta = %q, want a partly-empty size bar for the smaller member", metaText)
	}
	m1Meta := idx["member-1-meta"]
	meta1Text, _ := m1Meta["text"].(string)
	if !strings.Contains(meta1Text, strings.Repeat("█", 8)) {
		t.Fatalf("member-1-meta = %q, want a full size bar for the largest member", meta1Text)
	}

	// An A2UI host (Crush's read_mcp_resource) appends a ?w= width hint to
	// every /a2ui URI it reads. The bundle surface ignores the value, but the
	// SDK's {?w} template matching must still route the read — a template
	// registered without {?w} 404s on the hinted URI even though the handler
	// would strip the query. Both schemes must accept the hint.
	for _, uri := range []string{
		"mcp://cairn/bundle/" + createdBundle.ID + "/a2ui?w=120",
		"cairn://bundle/" + createdBundle.ID + "/a2ui?w=80",
	} {
		env := decodeA2UI(t, sess, uri)
		if _, ok := a2uiIndex(env)["member-0-name"]; !ok {
			t.Fatalf("width-hinted read %s missing member-0-name", uri)
		}
	}
}

// TestIntegrationMCPBundleMemberA2UI covers the bundle-member navigation
// target (story #104): reading cairn://bundle/{id}/{name}/a2ui renders one
// member as its own A2UI surface — header (member name, size, badge, media
// type) plus the markdown body via md2a2ui — under a per-member surfaceId
// distinct from the bundle surface. Both schemes and the ?w= width hint are
// accepted; nested member names travel percent-encoded (%2F).
func TestIntegrationMCPBundleMemberA2UI(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title": "the q3 audit",
		"members": []map[string]any{
			{"name": "README.md", "body": "# audit\n\n- findings", "media_type": "text/markdown"},
			{"name": "dir/main.go", "body": "package main", "media_type": "text/x-go"},
		},
	})
	var createdBundle mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdBundle)
	if createdBundle.ID == "" {
		t.Fatalf("bundle_create returned no id: %+v", createdBundle)
	}

	env := decodeA2UI(t, sess, "cairn://bundle/"+createdBundle.ID+"/README.md/a2ui")
	if env.Version != a2uiVersion {
		t.Fatalf("envelope version = %q, want %q", env.Version, a2uiVersion)
	}
	// Per-member surface id, distinct from the bundle surface id.
	if got, want := env.UpdateComponents.SurfaceID, a2uiMemberSurfaceID(createdBundle.ID, "README.md"); got != want {
		t.Fatalf("surfaceId = %q, want %q (distinct from bundle surface)", got, want)
	}

	idx := a2uiIndex(env)
	if got := idx["hdr-title"]["text"]; got != "README.md" {
		t.Fatalf("hdr-title = %v, want the member name", got)
	}
	// The markdown body is structured: the "# audit" heading becomes an h1.
	h1, ok := idx["md-text-1"]
	if !ok {
		t.Fatalf("missing md-text-1 (markdown heading): %+v", idx)
	}
	if v := h1["variant"]; v != "h1" {
		t.Fatalf("md-text-1 variant = %v, want h1", v)
	}

	// The mcp:// alias works too, as does the ?w= width hint on both schemes.
	for _, uri := range []string{
		"mcp://cairn/bundle/" + createdBundle.ID + "/README.md/a2ui",
		"cairn://bundle/" + createdBundle.ID + "/README.md/a2ui?w=120",
		"mcp://cairn/bundle/" + createdBundle.ID + "/README.md/a2ui?w=80",
	} {
		env := decodeA2UI(t, sess, uri)
		if _, ok := a2uiIndex(env)["md-text-1"]; !ok {
			t.Fatalf("read %s missing md-text-1 (markdown heading)", uri)
		}
	}

	// A nested member name travels percent-encoded (dir/main.go → dir%2Fmain.go);
	// a non-markdown member renders the plain-text body path.
	env = decodeA2UI(t, sess, "cairn://bundle/"+createdBundle.ID+"/dir%2Fmain.go/a2ui")
	idx = a2uiIndex(env)
	if got := idx["hdr-title"]["text"]; got != "dir/main.go" {
		t.Fatalf("nested member hdr-title = %v, want dir/main.go (percent-decoded)", got)
	}
	body, ok := idx["body-text"]
	if !ok || !strings.Contains(body["text"].(string), "package main") {
		t.Fatalf("nested member body-text = %v, want the Go source", body)
	}
}

// TestIntegrationMCPBundleMemberA2UIUnknown proves the member surface returns
// the store's uniform not-found for an unknown member name (and does not
// distinguish it from an unknown bundle), rather than an internal error.
func TestIntegrationMCPBundleMemberA2UIUnknown(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title":   "the q3 audit",
		"members": []map[string]any{{"name": "README.md", "body": "# audit", "media_type": "text/markdown"}},
	})
	var createdBundle mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdBundle)

	_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "cairn://bundle/" + createdBundle.ID + "/nosuch.md/a2ui",
	})
	if err == nil {
		t.Fatal("reading an unknown member must fail")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown member error = %v, want a uniform not-found", err)
	}
}

// TestIntegrationMCPA2UIActionRoundTrip covers story #106: calling a2ui_action
// with {name: open_member, context: {bundle, member}} returns an A2UI
// EmbeddedResource whose decoded envelope is the member surface — identical
// components to reading the member resource directly (#104) — plus a text
// fallback. a2ui_error is accepted. Both tools appear in tools/list.
func TestIntegrationMCPA2UIActionRoundTrip(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title":   "the q3 audit",
		"members": []map[string]any{{"name": "README.md", "body": "# audit\n\n- findings", "media_type": "text/markdown"}},
	})
	var createdBundle mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdBundle)

	// Capability check: both round-trip tools are advertised.
	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var sawAction, sawError bool
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "a2ui_action":
			sawAction = true
		case "a2ui_error":
			sawError = true
		}
	}
	if !sawAction || !sawError {
		t.Fatalf("round-trip tools not advertised: a2ui_action=%v a2ui_error=%v", sawAction, sawError)
	}

	// The round-trip: open_member returns the member's A2UI payload.
	res := callTool(t, sess, "a2ui_action", map[string]any{
		"name":    "open_member",
		"context": map[string]any{"bundle": createdBundle.ID, "member": "README.md"},
	})
	var embedded *mcp.EmbeddedResource
	var sawTextFallback bool
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.EmbeddedResource:
			embedded = v
		case *mcp.TextContent:
			sawTextFallback = true
		}
	}
	if embedded == nil {
		t.Fatalf("a2ui_action returned no EmbeddedResource: %+v", res.Content)
	}
	if !sawTextFallback {
		t.Fatal("a2ui_action must include a text fallback for non-A2UI callers")
	}
	if got := embedded.Resource.MIMEType; got != a2uiMIME {
		t.Fatalf("embedded MIME = %q, want %q", got, a2uiMIME)
	}

	var env a2uiEnvelope
	if err := json.Unmarshal([]byte(embedded.Resource.Text), &env); err != nil {
		t.Fatalf("decode embedded a2ui: %v\nbody: %s", err, embedded.Resource.Text)
	}
	// Same member surface as a direct resource read.
	direct := decodeA2UI(t, sess, "cairn://bundle/"+createdBundle.ID+"/README.md/a2ui")
	if env.UpdateComponents.SurfaceID != direct.UpdateComponents.SurfaceID {
		t.Fatalf("round-trip surfaceId = %q, want %q (same as resource read)",
			env.UpdateComponents.SurfaceID, direct.UpdateComponents.SurfaceID)
	}
	if _, ok := a2uiIndex(env)["md-text-1"]; !ok {
		t.Fatal("round-trip member surface missing md-text-1 (markdown heading)")
	}

	// a2ui_error is accepted as a sink.
	errRes := callTool(t, sess, "a2ui_error", map[string]any{
		"code": "render", "message": "boom", "surfaceId": env.UpdateComponents.SurfaceID,
	})
	if errRes.IsError {
		t.Fatalf("a2ui_error reported an error: %+v", errRes.Content)
	}
}

// TestIntegrationMCPA2UIActionValidation proves the unhappy paths: unknown
// action name, missing context keys, and unknown member each fail cleanly.
func TestIntegrationMCPA2UIActionValidation(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title":   "the q3 audit",
		"members": []map[string]any{{"name": "README.md", "body": "# audit", "media_type": "text/markdown"}},
	})
	var createdBundle mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdBundle)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"unknown action", map[string]any{"name": "delete_member", "context": map[string]any{"bundle": createdBundle.ID, "member": "README.md"}}},
		{"missing member key", map[string]any{"name": "open_member", "context": map[string]any{"bundle": createdBundle.ID}}},
		{"missing bundle key", map[string]any{"name": "open_member", "context": map[string]any{"member": "README.md"}}},
		{"unknown member", map[string]any{"name": "open_member", "context": map[string]any{"bundle": createdBundle.ID, "member": "nosuch.md"}}},
	} {
		res := callTool(t, sess, "a2ui_action", tc.args)
		if !res.IsError {
			t.Fatalf("%s: expected an error result, got %+v", tc.name, res.Content)
		}
	}
}

// TestIntegrationMCPBundleA2UIRejectsNonBundle proves the unhappy path for a
// wrong share type: reading the bundle a2ui resource against a single-body
// artifact id is a validation failure naming the actual share type, not a
// confusingly generic not-found. The bundle surface is honest about what
// it's for; an artifact id that doesn't name a bundle gets told so.
func TestIntegrationMCPBundleA2UIRejectsNonBundle(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "artifact_create", map[string]any{
		"title": "just a file",
		"body":  "hi",
	})
	var singleOut mcpCreateOutput
	decodeToolJSON(t, created, &singleOut)
	if singleOut.ID == "" {
		t.Fatalf("artifact_create returned no id: %+v", singleOut)
	}

	_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/bundle/" + singleOut.ID + "/a2ui",
	})
	if err == nil {
		t.Fatal("reading a bundle a2ui against a single-body artifact id must fail")
	}
	// The MCP surface's error convention is `code: generic_message` — see
	// mcpToolErr. Asserting on the code (not the specific "not a bundle"
	// text) keeps this test aligned with how every other resource error is
	// shaped here.
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Fatalf("non-bundle a2ui error = %v, want validation_failed", err)
	}
}

// TestIntegrationMCPBundleA2UIRequiresRead proves the bundle surface honours
// the same single artifacts:read scope as every other read.
func TestIntegrationMCPBundleA2UIRequiresRead(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	writeToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, writeToken, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"members": []map[string]any{{"name": "a.txt", "body": "hi"}},
	})
	var bundleOut mcpBundleCreateOutput
	decodeToolJSON(t, created, &bundleOut)

	// A write-only token must be refused.
	writeOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	writeSess := mcpClient(t, srv, writeOnly, nil, "writeonly-agent")
	_, err := writeSess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/bundle/" + bundleOut.ID + "/a2ui",
	})
	if err == nil {
		t.Fatal("write-only token must not read the a2ui bundle resource")
	}
}

// TestIntegrationMCPBundleA2UISingleMemberBundle covers the inverse of the
// empty-bundle case: a one-member bundle renders exactly one member card
// (and does NOT render the members-empty marker, which only fires when the
// member count is zero).
func TestIntegrationMCPBundleA2UISingleMemberBundle(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})

	sess := mcpClient(t, srv, token, nil, "a2ui-agent")
	created := callTool(t, sess, "bundle_create", map[string]any{
		"members": []map[string]any{{"name": "only.txt", "body": "x"}},
	})
	var bundleOut mcpBundleCreateOutput
	decodeToolJSON(t, created, &bundleOut)

	env := decodeA2UI(t, sess, "mcp://cairn/bundle/"+bundleOut.ID+"/a2ui")
	idx := a2uiIndex(env)
	if _, ok := idx["members-empty"]; ok {
		t.Fatalf("non-empty bundle must not render members-empty; got %+v", idx)
	}
	if _, ok := idx["member-0-name"]; !ok {
		t.Fatalf("missing member-0-name; got %+v", idx)
	}
}

// TestIntegrationMCPRunA2UINotSubscribable proves subscribing to an a2ui
// surface fails with an error naming the actual limitation and the JSON
// resource to subscribe to instead — not the run matcher's misleading "not a
// run resource URI" complaint about a URI the server itself advertises.
func TestIntegrationMCPRunA2UINotSubscribable(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	runID := openRunViaMCP(t, srv, token)

	sess := mcpClient(t, srv, token, nil, "a2ui-agent")
	for _, uri := range []string{
		"mcp://cairn/run/" + runID + "/a2ui",
		"cairn://run/" + runID + "/a2ui",
	} {
		err := sess.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri})
		if err == nil {
			t.Fatalf("subscribe %s must fail: a2ui surfaces are not subscribable", uri)
		}
		if !strings.Contains(err.Error(), "not subscribable") {
			t.Fatalf("subscribe %s error = %v, want it to say the surface is not subscribable", uri, err)
		}
	}
}

// TestIntegrationMCPRunA2UICairnScheme proves the cairn:// alias the issue
// spec calls out resolves to the same handler as the advertised mcp://
// template — a hand-written @-mention naming cairn://run/<id>/a2ui gets the
// same surface.
func TestIntegrationMCPRunA2UICairnScheme(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "run_create", map[string]any{
		"title": "cairn-scheme probe",
		"spans": []map[string]any{
			{"span_id": "s1", "category": "exec", "start_offset_ms": 0, "duration_ms": 10},
		},
	})
	var runOut mcpRunOutput
	decodeToolJSON(t, created, &runOut)

	env := decodeA2UI(t, sess, "cairn://run/"+runOut.ID+"/a2ui")
	idx := a2uiIndex(env)
	if got := idx["hdr-title"]["text"]; got != "cairn-scheme probe" {
		t.Fatalf("cairn:// run a2ui hdr-title = %v", got)
	}
}

// TestIntegrationMCPRunA2UIWidthHint proves the ?w= query survives the whole
// stack — the SDK's {?w} template matching, the query-tolerant URI matcher,
// and the renderer — by reading the same run at two widths and measuring the
// axis row (label column + gutter edges + gutter, no ANSI). This is the one
// place the RFC 6570 form-style template is exercised against a real
// resources/read, so a regression in SDK matching fails here, not in a
// host's terminal.
func TestIntegrationMCPRunA2UIWidthHint(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "run_create", map[string]any{
		"title": "width probe",
		"spans": []map[string]any{
			{"span_id": "s1", "category": "exec", "start_offset_ms": 0, "duration_ms": 100},
		},
	})
	var runOut mcpRunOutput
	decodeToolJSON(t, created, &runOut)

	axisCells := func(uri string) int {
		t.Helper()
		idx := a2uiIndex(decodeA2UI(t, sess, uri))
		axis, _ := idx["flame-axis"]["text"].(string)
		if axis == "" {
			t.Fatalf("read %s: no flame-axis row", uri)
		}
		return len([]rune(a2uiStripANSI(axis)))
	}

	// Default read: label + 2 edges + default gutter.
	if got, want := axisCells("mcp://cairn/run/"+runOut.ID+"/a2ui"), a2uiFlameLabelW+2+a2uiFlameGutterW; got != want {
		t.Fatalf("default axis = %d cells, want %d", got, want)
	}
	// ?w=120 sizes the total row: axis = 120 minus the duration column.
	if got, want := axisCells("mcp://cairn/run/"+runOut.ID+"/a2ui?w=120"), a2uiFlameLabelW+2+(120-a2uiWidthOverhead); got != want {
		t.Fatalf("?w=120 axis = %d cells, want %d", got, want)
	}
	// The cairn:// alias takes the hint too.
	if got, want := axisCells("cairn://run/"+runOut.ID+"/a2ui?w=80"), a2uiFlameLabelW+2+(80-a2uiWidthOverhead); got != want {
		t.Fatalf("cairn:// ?w=80 axis = %d cells, want %d", got, want)
	}
}

// TestIntegrationMCPArtifactA2UI covers the single-body artifact view: a
// markdown artifact created via artifact_create renders as an A2UI envelope
// with a header card (title, media type, visibility) and a body card carrying
// the artifact's text content.
func TestIntegrationMCPArtifactA2UI(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "artifact_create", map[string]any{
		"title":      "gap analysis",
		"body":       "# Gaps\n\n- OpenBao HA missing\n- Cert renewal untracked",
		"media_type": "text/markdown",
	})
	var artOut mcpCreateOutput
	decodeToolJSON(t, created, &artOut)
	if artOut.ID == "" {
		t.Fatalf("artifact_create returned no id: %+v", artOut)
	}

	env := decodeA2UI(t, sess, "mcp://cairn/artifact/"+artOut.ID+"/a2ui")
	if env.Version != a2uiVersion {
		t.Fatalf("envelope version = %q, want %q", env.Version, a2uiVersion)
	}

	idx := a2uiIndex(env)

	// Header carries the title.
	title, ok := idx["hdr-title"]
	if !ok || title["text"] != "gap analysis" {
		t.Fatalf("hdr-title = %+v, want the artifact title", title)
	}

	// Header title uses the spec-defined "variant" field, not "usageHint".
	if v, ok := idx["hdr-title"]["variant"]; !ok || v != "h2" {
		t.Fatalf("hdr-title variant = %v, want %q", v, "h2")
	}
	if _, ok := idx["hdr-title"]["usageHint"]; ok {
		t.Fatal("hdr-title must NOT carry usageHint")
	}

	// Body card carries the text content. For markdown artifacts, the body
	// is rendered through md2a2ui as structured components — the heading
	// "# Gaps" becomes an h1 Text with the heading text, and each list item
	// becomes a body Text inside a List.
	h1, ok := idx["md-text-1"]
	if !ok {
		t.Fatalf("missing md-text-1 (markdown heading component): %+v", idx)
	}
	if v, ok := h1["variant"]; !ok || v != "h1" {
		t.Fatalf("md-text-1 variant = %v, want %q", v, "h1")
	}
	h1Text, _ := h1["text"].(string)
	if !strings.Contains(h1Text, "Gaps") {
		t.Fatalf("md-text-1 = %q, want it to contain the heading text", h1Text)
	}
	// At least one list item body text carries "OpenBao HA missing".
	found := false
	for _, c := range idx {
		if text, _ := c["text"].(string); strings.Contains(text, "OpenBao HA missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no component contains 'OpenBao HA missing': %+v", idx)
	}

	// The cairn:// alias works too.
	env2 := decodeA2UI(t, sess, "cairn://artifact/"+artOut.ID+"/a2ui")
	idx2 := a2uiIndex(env2)
	if _, ok := idx2["md-text-1"]; !ok {
		t.Fatalf("cairn:// alias missing md-text-1 (markdown heading)")
	}

	// An A2UI host (Crush's read_mcp_resource) appends a ?w= width hint to
	// every /a2ui URI it reads. The artifact surface ignores the value, but
	// the SDK's {?w} template matching must still route the read — a template
	// registered without {?w} 404s on the hinted URI even though the handler
	// would strip the query. Both schemes must accept the hint.
	for _, uri := range []string{
		"mcp://cairn/artifact/" + artOut.ID + "/a2ui?w=120",
		"cairn://artifact/" + artOut.ID + "/a2ui?w=80",
	} {
		env := decodeA2UI(t, sess, uri)
		if _, ok := a2uiIndex(env)["md-text-1"]; !ok {
			t.Fatalf("width-hinted read %s missing md-text-1 (markdown heading)", uri)
		}
	}
}

// TestIntegrationMCPA2UIUnknownIDs proves the artifact and bundle surfaces
// surface the store's uniform not-found for an unknown id — matching what the
// run surface already asserts — rather than an internal error or a confusing
// validation complaint.
func TestIntegrationMCPA2UIUnknownIDs(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	for _, uri := range []string{
		"mcp://cairn/artifact/unknownid999/a2ui",
		"mcp://cairn/bundle/unknownid999/a2ui",
	} {
		_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err == nil {
			t.Fatalf("read %s must fail for an unknown id", uri)
		}
		if !strings.Contains(err.Error(), "not_found") {
			t.Fatalf("read %s error = %v, want the uniform not_found", uri, err)
		}
	}
}

// TestIntegrationMCPArtifactA2UITruncatesBody covers the a2uiMaxBodyBytes
// boundary: a body one byte over the cap is truncated with the marker, the cut
// never splits a multi-byte rune (no U+FFFD on the card), and a body exactly
// at the cap renders whole with no marker.
func TestIntegrationMCPArtifactA2UITruncatesBody(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	const marker = "…(truncated — read the full artifact via artifact_read)"

	// "é" (2 bytes) straddles the cap: bytes 8191..8192. A byte-index slice
	// at 8192 would tear it into invalid UTF-8; the rune-safe cut drops it.
	body := strings.Repeat("a", a2uiMaxBodyBytes-1) + "é" + strings.Repeat("b", 64)
	created := callTool(t, sess, "artifact_create", map[string]any{
		"title":      "big body",
		"body":       body,
		"media_type": "text/plain",
	})
	var artOut mcpCreateOutput
	decodeToolJSON(t, created, &artOut)
	if artOut.ID == "" {
		t.Fatalf("artifact_create returned no id: %+v", artOut)
	}

	env := decodeA2UI(t, sess, "mcp://cairn/artifact/"+artOut.ID+"/a2ui")
	idx := a2uiIndex(env)
	text, _ := idx["body-text"]["text"].(string)
	if !strings.Contains(text, marker) {
		t.Fatalf("body-text does not carry the truncation marker; len=%d", len(text))
	}
	if !utf8.ValidString(text) {
		t.Fatal("truncated body-text is not valid UTF-8")
	}
	if strings.ContainsRune(text, utf8.RuneError) {
		t.Fatal("truncated body-text carries U+FFFD — the cut split a rune")
	}
	if strings.ContainsRune(text, 'é') {
		t.Fatalf("the straddling rune must be dropped by the rune-safe cut")
	}

	// Exactly at the cap: no truncation, no marker.
	exact := strings.Repeat("x", a2uiMaxBodyBytes)
	created = callTool(t, sess, "artifact_create", map[string]any{
		"title":      "exact body",
		"body":       exact,
		"media_type": "text/plain",
	})
	var exactOut mcpCreateOutput
	decodeToolJSON(t, created, &exactOut)

	env = decodeA2UI(t, sess, "mcp://cairn/artifact/"+exactOut.ID+"/a2ui")
	idx = a2uiIndex(env)
	text, _ = idx["body-text"]["text"].(string)
	if text != exact {
		t.Fatalf("exactly-at-cap body must render whole; got len=%d want len=%d", len(text), len(exact))
	}
	if strings.Contains(text, marker) {
		t.Fatal("exactly-at-cap body must not carry the truncation marker")
	}
}

// TestIntegrationMCPArtifactA2UIRejectsBodyless proves the unhappy path:
// reading a bundle via the artifact a2ui surface is a validation failure
// naming the actual share type, directing the caller to the correct surface.
func TestIntegrationMCPArtifactA2UIRejectsBodyless(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"members": []map[string]any{{"name": "a.txt", "body": "hi"}},
	})
	var bundleOut mcpBundleCreateOutput
	decodeToolJSON(t, created, &bundleOut)

	_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/artifact/" + bundleOut.ID + "/a2ui",
	})
	if err == nil {
		t.Fatal("reading a bundle via the artifact a2ui surface must fail")
	}
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Fatalf("bodyless artifact error = %v, want validation_failed", err)
	}
}

// TestIntegrationMCPArtifactA2UIRejectsBinaryBody proves the registered
// contract ("Binary artifacts (images, gz) are rejected — they have no text
// to render"): an image artifact created via the REST web upload path (the
// binary-body route MCP's artifact_create lacks) is refused with a validation
// failure rather than rendered as mojibake text.
func TestIntegrationMCPArtifactA2UIRejectsBinaryBody(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})

	// The OAuth access token authenticates on /v1 through the same bearer
	// chain the MCP endpoint uses, so the upload happens as the same human.
	id := createImageArtifact(t, srv.URL, token, "screenshot.png", onePxPNG)

	sess := mcpClient(t, srv, token, nil, "a2ui-agent")
	_, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/artifact/" + id + "/a2ui",
	})
	if err == nil {
		t.Fatal("reading a binary-body artifact via the a2ui surface must fail")
	}
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Fatalf("binary body a2ui error = %v, want validation_failed", err)
	}
}

// TestIntegrationMCPArtifactA2UIRequiresRead proves the artifact surface
// honours the same single artifacts:read scope as every other read.
func TestIntegrationMCPArtifactA2UIRequiresRead(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	writeToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, writeToken, nil, "a2ui-agent")

	created := callTool(t, sess, "artifact_create", map[string]any{
		"title": "scoped probe",
		"body":  "hi",
	})
	var artOut mcpCreateOutput
	decodeToolJSON(t, created, &artOut)

	writeOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	writeSess := mcpClient(t, srv, writeOnly, nil, "writeonly-agent")
	_, err := writeSess.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "mcp://cairn/artifact/" + artOut.ID + "/a2ui",
	})
	if err == nil {
		t.Fatal("write-only token must not read the a2ui artifact resource")
	}
}
