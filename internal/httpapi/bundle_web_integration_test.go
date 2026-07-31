package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// bundleMember is one file placed in a test bundle.
type bundleMember struct{ name, body string }

// createBundle posts a multipart bundle (N `file` parts → the store's
// CreateBundle path) and returns its public id. Members keep their upload order,
// so members[0] is the bundle's first (initially active) member.
func createBundle(t *testing.T, srvURL, actor, title string, members []bundleMember) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", title)
	for _, m := range members {
		fw, err := mw.CreateFormFile("file", m.name)
		if err != nil {
			t.Fatalf("form file %q: %v", m.name, err)
		}
		fw.Write([]byte(m.body))
	}
	mw.Close()
	resp := do(t, http.MethodPost, srvURL+"/v1/artifacts", actor, &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bundle create = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.ShareType != "bundle" {
		t.Fatalf("share_type = %q, want bundle", art.ShareType)
	}
	return art.ID
}

const bundleMD = "# Release Notes\n\nShipping the **bundle viewer**.\n\n## Highlights\n\n- file rail\n- per-file comments\n"

// TestIntegrationBundleViewer is the story's headline acceptance (SPEC-0003,
// issue #19): a bundle browses via the file rail, the active member renders
// through the registry (markdown → the M1 viewer), a non-md member degrades to
// the generic-file card, and the rail carries listbox/tab a11y semantics.
func TestIntegrationBundleViewer(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "my bundle", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}

	// The bundle viewer replaced the whole-artifact generic-file floor: the rail
	// header, both member names, and their type badges are present.
	for _, frag := range []string{
		`class="bundle-viewer"`,
		`2 FILES · `, // rail header: N FILES · total size
		`data-file-name="notes.md"`,
		`data-file-name="data.bin"`,
		`role="tablist"`, // rail as tablist semantics
		`role="tab"`,
		`aria-controls="bundle-pane"`,
		`role="tabpanel"`,                // the pane
		`aria-labelledby="bundle-tab-0"`, // pane labelled by the active file
		`bundle.js`,                      // keyboard-nav enhancement wired in
		`id="contains-h"`,                // the CONTAINS metadata-panel section
		`class="bundle-contains"`,        // the member manifest list
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("bundle shell for %s missing %q", id, frag)
		}
	}

	// The first member (notes.md) is active and renders through the registry
	// markdown viewer (delegation), not the generic-file card.
	if !strings.Contains(html, `class="md-viewer"`) {
		t.Error("active markdown member should render the M1 markdown viewer in the pane")
	}
	if !strings.Contains(html, "<h1>Release Notes</h1>") {
		t.Error("markdown member body should be rendered in the pane")
	}
	// The active tab is selected with a roving tabindex of 0; the other is -1.
	if !strings.Contains(html, `aria-selected="true"`) || !strings.Contains(html, `aria-selected="false"`) {
		t.Error("rail must carry a single selected tab and unselected siblings")
	}
}

// TestIntegrationBundlePaneSwap asserts the HTMX pane route renders just one
// member's pane, delegating a non-md member to the generic-file card with a
// same-origin member download (SPEC-0003 "non-md members degrade to the file
// card"; "HTMX pane swap, no full reload").
func TestIntegrationBundlePaneSwap(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "mixed", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	// The non-md member's pane is the generic-file card with a member download.
	status, html := getHTML(t, srv.URL+"/"+id+"/pane/data.bin")
	if status != http.StatusOK {
		t.Fatalf("pane swap = %d, want 200", status)
	}
	if !strings.Contains(html, "generic-card") {
		t.Error("non-md member pane should be the generic-file card")
	}
	if !strings.Contains(html, "/"+id+"/members/data.bin") {
		t.Error("file-card pane should link a same-origin member download")
	}
	// It is a fragment, not a whole page.
	if strings.Contains(html, "<!doctype html>") {
		t.Error("pane swap must return a fragment, not a full document")
	}

	// The markdown member's pane is the rich viewer fragment.
	_, mdHTML := getHTML(t, srv.URL+"/"+id+"/pane/notes.md")
	if !strings.Contains(mdHTML, `class="md-viewer"`) {
		t.Error("markdown member pane should render the markdown viewer")
	}

	// An unknown member is a uniform 404.
	status, _ = getHTML(t, srv.URL+"/"+id+"/pane/missing.txt")
	if status != http.StatusNotFound {
		t.Errorf("unknown member pane = %d, want 404", status)
	}
}

// TestIntegrationBundleActiveFileNav asserts the no-JS `?file=` full-navigation
// path selects the requested member as active (progressive enhancement: reading
// works with server HTML alone).
func TestIntegrationBundleActiveFileNav(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "mixed", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	_, html := getHTML(t, srv.URL+"/"+id+"?file=data.bin")
	// data.bin (index 1) is now the active pane → its card renders and the pane is
	// labelled by tab 1.
	if !strings.Contains(html, "generic-card") {
		t.Error("?file=data.bin should render the data.bin file card in the pane")
	}
	if !strings.Contains(html, `aria-labelledby="bundle-tab-1"`) {
		t.Error("pane should be labelled by the active (data.bin) tab")
	}
}

// TestIntegrationBundlePerFileComment asserts a comment scoped to a member
// (the composer's `file` field) anchors to bundle_file, renders back in the
// aggregated COMMENTS panel with its per-file context label, and bumps that
// member's engagement count in the rail (SPEC-0003 REQ "Bundle Viewer":
// per-file annotation resolves to the member; SPEC-0006 anchors).
func TestIntegrationBundlePerFileComment(t *testing.T) {
	srv, client := sessionServer(t)
	id := createBundle(t, srv.URL, "joe", "mixed", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
		"body": {"rename this section"},
		"file": {"notes.md"},
	}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post per-file comment = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, "rename this section") {
		t.Error("per-file comment body should render in the panel")
	}
	if !strings.Contains(html, "on notes.md") {
		t.Error("per-file comment should carry its member context label")
	}
	// The rail shows notes.md's engagement count (1 comment) as a badge.
	if !strings.Contains(html, `class="bundle-file-count"`) {
		t.Error("rail should show a per-file engagement count for the annotated member")
	}
}

// TestIntegrationBundleMemberBulletReactionsAreDistinct is the regression for
// the bug that made reacting inside a bundled markdown file look broken: the
// bundle_file locator carried only {name, block_id}, so every bullet of a list
// collapsed onto its BLOCK's anchor. Reacting to the second bullet re-posted
// the anchor the first one had already created, the server (correctly)
// no-oped the duplicate with 200, and the reader saw nothing happen.
//
// A markdown member renders the same blocks and bullets as the standalone
// markdown viewer, so it must anchor them just as finely: each bullet is its
// own {name, block_id, path} anchor, each a real create, each its own tally.
// Governing: SPEC-0003 REQ "Bundle Viewer", SPEC-0006 REQ "Polymorphic Anchor
// Model".
func TestIntegrationBundleMemberBulletReactionsAreDistinct(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "bullets", []bundleMember{
		{"notes.md", bundleMD}, // "## Highlights" over a two-item list
		{"data.bin", "\x00\x01binary blob"},
	})

	_, html := getHTML(t, srv.URL+"/"+id)
	listBlock := listBlockIDIn(t, html)

	react := func(ref string) int {
		resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
			jsonReader(t, reactionRequest{AnchorType: "bundle_file", AnchorRef: json.RawMessage(ref), Emoji: "👍"}),
			"application/json")
		resp.Body.Close()
		return resp.StatusCode
	}

	first := `{"name":"notes.md","block_id":"` + listBlock + `","path":[0]}`
	second := `{"name":"notes.md","block_id":"` + listBlock + `","path":[1]}`
	if got := react(first); got != http.StatusCreated {
		t.Fatalf("react first bullet = %d, want 201", got)
	}
	// The pre-fix failure mode lands exactly here: a 200 means the server
	// treated the second bullet as a repeat of the first.
	if got := react(second); got != http.StatusCreated {
		t.Fatalf("react second bullet = %d, want 201 (a 200 means both bullets share one anchor)", got)
	}

	tallies := decodeTallies(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice", nil, ""))
	if len(tallies.Reactions) != 2 {
		t.Fatalf("tallies = %+v, want one per bullet", tallies.Reactions)
	}
	paths := map[string]bool{}
	for _, tly := range tallies.Reactions {
		var key struct {
			Name    string `json:"name"`
			BlockID string `json:"block_id"`
			Path    []int  `json:"path"`
		}
		if err := json.Unmarshal([]byte(tly.AnchorKey), &key); err != nil {
			t.Fatalf("anchor_key %q did not parse as JSON: %v", tly.AnchorKey, err)
		}
		if key.Name != "notes.md" || key.BlockID != listBlock {
			t.Errorf("anchor_key = %+v, want name=notes.md block_id=%q", key, listBlock)
		}
		if tly.Count != 1 {
			t.Errorf("tally %s count = %d, want 1 per bullet", tly.AnchorKey, tly.Count)
		}
		paths[fmt.Sprint(key.Path)] = true
	}
	if !paths["[0]"] || !paths["[1]"] {
		t.Errorf("bullet paths = %v, want distinct [0] and [1]", paths)
	}

	// Both bullets roll up into the member's rail engagement count.
	_, html = getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, `data-file-name="notes.md" data-reactions="2" data-comments="0"`) {
		t.Error("rail row should aggregate both bullet reactions")
	}
}

// TestIntegrationBundleMemberBlockReactionRoundTrip asserts the (#72) server-side
// contract bundle.js's click-time feedback depends on: a reaction posted
// against a real rendered block INSIDE a bundle member anchors as bundle_file
// {name, block_id} (not md_block — the bundle forbids that anchor on a member's
// own blocks, SPEC-0006), round-trips through GET /v1/artifacts/{id}/reactions,
// and — because bundle.js's rail badge update is optimistic client state, not
// server-pushed — a subsequent server render (what a reload, or any HTMX pane
// swap that re-renders the shell, would show) reflects the same count via the
// data-reactions/data-comments attributes bundle.js reads to compute its delta.
// Toggling the same emoji off (DELETE) removes the tally entirely and the rail
// count reverts, the same idempotent contract #66 established for markdown.js.
func TestIntegrationBundleMemberBlockReactionRoundTrip(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "mixed", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, `data-file-name="notes.md" data-reactions="0" data-comments="0"`) {
		t.Fatal("baseline rail row should carry zeroed data-reactions/data-comments for bumpFileEngagement to read")
	}
	blockID := blockIDsIn(html)[0] // notes.md (the active member)'s first block

	ref := `{"name":"notes.md","block_id":"` + blockID + `"}`
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "bundle_file", AnchorRef: json.RawMessage(ref), Emoji: "👍"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("react bundle_file = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	tallies := decodeTallies(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice", nil, ""))
	if len(tallies.Reactions) != 1 {
		t.Fatalf("tallies = %+v, want 1", tallies.Reactions)
	}
	tly := tallies.Reactions[0]
	var key struct {
		Name    string `json:"name"`
		BlockID string `json:"block_id"`
	}
	if err := json.Unmarshal([]byte(tly.AnchorKey), &key); err != nil {
		t.Fatalf("anchor_key %q did not parse as JSON: %v", tly.AnchorKey, err)
	}
	if tly.AnchorType != "bundle_file" || tly.Emoji != "👍" || tly.Count != 1 || !tly.Reacted {
		t.Errorf("tally = %+v, want bundle_file 👍/1/reacted", tly)
	}
	if key.Name != "notes.md" || key.BlockID != blockID {
		t.Errorf("anchor_key = %+v, want name=notes.md block_id=%q", key, blockID)
	}

	// A server render after the react shows the rail's per-member counters
	// bumped — the state bundle.js's optimistic bumpFileEngagement mirrors at
	// click-time so the count is never stale even before any reload happens.
	_, html = getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, `data-file-name="notes.md" data-reactions="1" data-comments="0"`) {
		t.Error("rail row should carry data-reactions=1 after the react")
	}
	if !strings.Contains(html, `class="bundle-file-count" title="1 reactions · 0 comments" aria-label="1 annotations">1<`) {
		t.Error("rail badge should render the bumped engagement count")
	}
	// data.bin is untouched.
	if !strings.Contains(html, `data-file-name="data.bin" data-reactions="0" data-comments="0"`) {
		t.Error("unreacted member's rail row must stay at zero")
	}

	// Toggling the same emoji off (DELETE) removes the tally entirely — the
	// idempotent unreact contract #66 established for markdown.js — and the
	// rail count reverts on the next server render.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "bundle_file", AnchorRef: json.RawMessage(ref), Emoji: "👍"}),
		"application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unreact bundle_file = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	tallies = decodeTallies(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice", nil, ""))
	if len(tallies.Reactions) != 0 {
		t.Fatalf("tallies after unreact = %+v, want none", tallies.Reactions)
	}
	_, html = getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, `data-file-name="notes.md" data-reactions="0" data-comments="0"`) {
		t.Error("rail row should revert to data-reactions=0 after the unreact")
	}
	if strings.Contains(html, `class="bundle-file-count"`) {
		t.Error("rail badge should be gone once engagement returns to zero")
	}
}

// TestIntegrationBundleMemberDownload asserts a member downloads as a sniff-proof
// same-origin attachment whose bytes round-trip (SPEC-0003 Security REQ
// "Sniff-proof raw body").
func TestIntegrationBundleMemberDownload(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "mixed", []bundleMember{
		{"notes.md", bundleMD},
		{"data.bin", "\x00\x01binary blob"},
	})

	resp := do(t, http.MethodGet, srv.URL+"/"+id+"/members/data.bin", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member download = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("member content-type = %q, want application/octet-stream", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("member disposition = %q, want attachment", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("member download missing nosniff header")
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "\x00\x01binary blob" {
		t.Errorf("member bytes = %q, want the uploaded blob", got)
	}
}

// TestIntegrationBundleRendersSourceMembersInline is the regression for a
// bundle of source files rendering as nothing but download cards. Only markdown
// was ever classified as a viewable member, so the single most obvious reason
// to build a bundle — sharing a set of related source files — produced the
// least useful page in the product.
//
// It also pins the second half of that fix: RenderBody is handed the BUNDLE
// artifact, so a member whose language was detected from the artifact's own
// media type/title rendered as "Plain Text" however obvious its filename. The
// member's language now travels through the body-hint seam.
//
// Governing: SPEC-0003 REQ "Bundle Viewer" ("delegate the selected member to
// that file's registered viewer")
func TestIntegrationBundleRendersSourceMembersInline(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createBundle(t, srv.URL, "joe", "source bundle", []bundleMember{
		{name: "trajectory.go", body: "package trajectory\n\nfunc Run() {}\n"},
		{name: "styles.css", body: ".a { color: red; }\n"},
		{name: "data.bin", body: "\x00\x01binary"},
	})

	for _, tc := range []struct{ file, wantLang string }{
		{"trajectory.go", "Go ·"},
		{"styles.css", "CSS ·"},
	} {
		status, html := getHTML(t, srv.URL+"/"+id+"?file="+tc.file)
		if status != http.StatusOK {
			t.Fatalf("GET member %s = %d, want 200", tc.file, status)
		}
		if !strings.Contains(html, "code-line-cell") {
			t.Errorf("%s must render through the code viewer, not a download card", tc.file)
		}
		if !strings.Contains(html, tc.wantLang) {
			t.Errorf("%s must detect its own language (want %q); a member used to inherit the bundle's", tc.file, tc.wantLang)
		}
		if strings.Contains(html, "Plain Text ·") {
			t.Errorf("%s rendered as Plain Text — the member's language did not reach the viewer", tc.file)
		}
	}

	// A member nothing can highlight still degrades to the generic file card.
	status, html := getHTML(t, srv.URL+"/"+id+"?file=data.bin")
	if status != http.StatusOK {
		t.Fatalf("GET binary member = %d, want 200", status)
	}
	if strings.Contains(html, "code-line-cell") {
		t.Error("a binary member must not be forced through the code viewer")
	}
}
