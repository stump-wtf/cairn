package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// markdownDoc is a small document exercising headings (for the TOC), a list (for
// the md_bullet affordance), prose, and — critically — an injection attempt.
const markdownDoc = "# Checkout Web Audit\n\n" +
	"An audit of the **checkout** flow.\n\n" +
	"## Findings\n\n" +
	"- missing CSRF token\n" +
	"- weak session cookie\n\n" +
	"## Remediation\n\n" +
	"Rotate the [session key](https://example.com/keys).\n\n" +
	"<script>alert('xss')</script>\n\n" +
	"<img src=x onerror=\"alert(1)\">\n"

// TestIntegrationMarkdownViewer is the story's headline acceptance (SPEC-0003,
// issue #18): a pushed .md renders in the shell through the registry BodyViewer
// capability with a working multi-level TOC, deterministic per-block ids, the
// md_block react affordance, and sanitized output (no script/handler survives).
func TestIntegrationMarkdownViewer(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "joe", markdownDoc)

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}

	// The rich markdown viewer replaced the generic-file floor.
	if strings.Contains(html, "generic-card") {
		t.Error("markdown artifact should render the rich viewer, not the generic-file card")
	}

	// Rendered viewer structure + annotation affordances (SPEC-0003 REQ
	// "Markdown Viewer", REQ "Markdown Annotation Anchors").
	for _, frag := range []string{
		`class="md-viewer"`,
		`aria-label="Table of contents"`, // TOC nav
		`href="#b_`,                      // TOC in-page link to a heading block
		`data-block-id="b_`,              // deterministic block ids
		`data-anchor-type="md_block"`,    // react affordance anchor
		`aria-label="React to this block"`,
		`<h1>Checkout Web Audit</h1>`,     // rendered heading
		`<strong>checkout</strong>`,       // rendered emphasis
		`data-md-list="true"`,             // list block flagged for md_bullet
		`href="https://example.com/keys"`, // safe link preserved
		`markdown.js`,                     // enhancement script wired in
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("markdown shell for %s missing %q", id, frag)
		}
	}

	// Sanitization: no active content from the body survives (SPEC-0003 Security
	// REQ "Untrusted markdown body"). (The shell's own <script defer src> asset
	// tags are legitimate, so we assert on the injected payload's fingerprints,
	// which have no legitimate occurrence.)
	for _, bad := range []string{"<script>alert", "onerror", "alert("} {
		if strings.Contains(html, bad) {
			t.Errorf("markdown shell leaked unsanitized %q into the page", bad)
		}
	}
}

// TestIntegrationMarkdownBlockIDsStable asserts the same body renders the same
// block ids on two requests, so a block reaction or comment survives re-render
// (ADR-0006, SPEC-0003 REQ "Markdown Viewer": "Stable block ids").
func TestIntegrationMarkdownBlockIDsStable(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "joe", markdownDoc)

	_, first := getHTML(t, srv.URL+"/"+id)
	_, second := getHTML(t, srv.URL+"/"+id)

	firstIDs := blockIDsIn(first)
	if len(firstIDs) == 0 {
		t.Fatal("expected at least one rendered block id")
	}
	secondIDs := blockIDsIn(second)
	if strings.Join(firstIDs, ",") != strings.Join(secondIDs, ",") {
		t.Errorf("block ids drifted between renders:\n  %v\n  %v", firstIDs, secondIDs)
	}
}

// TestIntegrationMarkdownSelectionComment asserts a text_selection comment
// posted through the shell's web composer (the anchor the markdown viewer emits)
// persists and renders back in the panel with its quote context (SPEC-0003 REQ
// "Markdown Annotation Anchors": select-to-comment; SPEC-0006 anchors).
func TestIntegrationMarkdownSelectionComment(t *testing.T) {
	srv, client := sessionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "joe", markdownDoc)

	// A logged-in web session posts a text_selection comment through the same
	// CSRF-guarded web route the viewer's selection composer submits to
	// (sel_start/sel_end/quote are exactly the hidden fields markdown.js sets).
	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
		"body":      {"needs rotating"},
		"sel_start": {"0"},
		"sel_end":   {"11"},
		"quote":     {"session key"},
	}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post selection comment = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, "needs rotating") {
		t.Error("selection comment body should render in the panel")
	}
	if !strings.Contains(html, "session key") {
		t.Error("selection comment should carry its quoted-substring anchor context")
	}
}

// blockIDsIn extracts the ordered list of data-block-id values from rendered
// HTML.
func blockIDsIn(html string) []string {
	var ids []string
	const marker = `data-block-id="`
	i := 0
	for {
		j := strings.Index(html[i:], marker)
		if j < 0 {
			break
		}
		start := i + j + len(marker)
		end := strings.IndexByte(html[start:], '"')
		if end < 0 {
			break
		}
		ids = append(ids, html[start:start+end])
		i = start + end
	}
	return ids
}
