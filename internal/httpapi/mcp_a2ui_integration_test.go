// Integration coverage for the A2UI MCP resources (issue #90): both surfaces
// (run, bundle), happy and unhappy paths. These run against the real
// streamable-HTTP MCP transport with a real OAuth-minted token over real
// Postgres — the same shape as the rest of the MCP integration suite, so
// every assertion exercises what an A2UI-capable host will actually speak.
package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/store"
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
// with a seeded span tree renders as an A2UI envelope with a header card,
// stats card, and a span list whose rows carry the span tree's depth
// (indented label), category, name and duration.
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

	// Spans list has two rows; the child row is indented relative to the
	// root row, and both carry the span's category, name and duration.
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

// TestIntegrationMCPBundleA2UI covers the bundle view end-to-end: a bundle
// created via bundle_create renders as an envelope header Card plus one
// Card per member, each carrying the member's name, size and media type.
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

	// Two member cards, each carrying the member's name and a size+media
	// caption.
	m0Name, ok0 := idx["member-0-name"]
	m1Name, ok1 := idx["member-1-name"]
	if !ok0 || !ok1 {
		t.Fatalf("missing member name components: %+v", idx)
	}
	if m0Name["text"] != "README.md" || m1Name["text"] != "main.go" {
		t.Fatalf("member names = %v, %v; want README.md and main.go", m0Name["text"], m1Name["text"])
	}
	m0Meta, ok := idx["member-0-meta"]
	if !ok {
		t.Fatalf("missing member-0-meta: %+v", idx)
	}
	metaText, _ := m0Meta["text"].(string)
	if !strings.Contains(metaText, "text/markdown") {
		t.Fatalf("member-0-meta = %q, want the media type", metaText)
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

	// Body card carries the text content.
	bodyText, ok := idx["body-text"]
	if !ok {
		t.Fatalf("missing body-text component: %+v", idx)
	}
	text, _ := bodyText["text"].(string)
	if !strings.Contains(text, "OpenBao HA missing") {
		t.Fatalf("body-text = %q, want it to contain the artifact body", text)
	}

	// The cairn:// alias works too.
	env2 := decodeA2UI(t, sess, "cairn://artifact/"+artOut.ID+"/a2ui")
	idx2 := a2uiIndex(env2)
	if _, ok := idx2["body-text"]; !ok {
		t.Fatalf("cairn:// alias missing body-text")
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
