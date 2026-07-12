package httpapi

import (
	"bytes"
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
