package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// codeDoc is a small Go source exercising a func, a struct, and a method (for
// the symbol outline) plus an injection attempt in a comment (for the
// XSS-safety assertion) — the code-viewer analogue of markdownDoc.
const codeDoc = "package sample\n\n" +
	"// <script>alert('xss')</script>\n" +
	"func Greet(name string) string {\n" +
	"\treturn \"hello \" + name\n" +
	"}\n\n" +
	"type Server struct {\n" +
	"\tName string\n" +
	"}\n\n" +
	"func (s *Server) Start() error {\n" +
	"\treturn nil\n" +
	"}\n"

// createArtifactWithTitle seeds an artifact with an explicit title (so a code
// artifact's language can be detected from its file extension, SPEC-0003 REQ
// "Code Viewer": "Language detected from media_type/title/extension") and
// media type, returning its public id.
func createArtifactWithTitle(t *testing.T, srvURL, shareType, actor, body, title, contentType string) string {
	t.Helper()
	resp := do(t, http.MethodPost,
		srvURL+"/v1/artifacts?type="+shareType+"&title="+url.QueryEscape(title),
		actor, strings.NewReader(body), contentType)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed %s artifact: status = %d, want 201", shareType, resp.StatusCode)
	}
	return decodeArtifact(t, resp).ID
}

// TestIntegrationCodeViewer is the story's headline acceptance (SPEC-0003,
// issue #68): a pushed source file renders in the shell through the registry
// BodyViewer capability with server-side syntax highlighting, line numbers, a
// symbol outline, the code_line react affordance, and sanitized output (no
// script/handler from the source survives).
func TestIntegrationCodeViewer(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifactWithTitle(t, srv.URL, "code", "joe", codeDoc, "sample.go", "text/plain")

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}

	if strings.Contains(html, "generic-card") {
		t.Error("code artifact should render the rich viewer, not the generic-file card")
	}

	for _, frag := range []string{
		`class="code-viewer"`,
		`aria-label="Symbol outline"`,   // outline nav
		`code-outline-link" href="#L4"`, // outline entry jumps to Greet's line
		`>Greet<`,                       // outline names the symbol
		`>Server<`,
		`>Start<`,
		`id="L1"`, // line-numbered rows
		`data-line="1"`,
		`aria-label="Line 1"`,
		`data-anchor-type="code_line"`, // react affordance anchor
		`aria-label="React to line 1"`,
		`aria-label="Comment on line 1"`, // per-line comment affordance
		`chroma-`,                        // server-side syntax highlighting applied
		`Go · `,                          // language + line-count stats
		`code.js`,                        // enhancement script wired in
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("code shell for %s missing %q", id, frag)
		}
	}

	// Sanitization: no active content from the source survives (SPEC-0003
	// Security Requirements: "source is escaped, never executed"). The shell's
	// own <script defer src> asset tags are legitimate, so we assert on the
	// injected payload's fingerprint, which has no legitimate occurrence.
	if strings.Contains(html, "<script>alert") {
		t.Errorf("code shell for %s leaked unescaped %q into the page", id, "<script>alert")
	}
}

// TestIntegrationCodeLanguageDetection asserts the language is resolved from
// the title's file extension when the media type is generic, from the media
// type when the title has none, and shown both in the viewer's STATS line and
// the registry MetadataPanel field (SPEC-0003 REQ "Code Viewer": "Language
// detected from media_type/title/extension").
func TestIntegrationCodeLanguageDetection(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	cases := []struct {
		title, contentType, body, wantLang string
	}{
		{"app.py", "text/plain", "def greet():\n    pass\n", "Python"},
		{"", "text/x-ruby", "def greet\nend\n", "Ruby"},
	}
	for _, tc := range cases {
		id := createArtifactWithTitle(t, srv.URL, "code", "joe", tc.body, tc.title, tc.contentType)
		_, html := getHTML(t, srv.URL+"/"+id)
		if !strings.Contains(html, tc.wantLang+" · ") {
			t.Errorf("title=%q contentType=%q: STATS line missing language %q; html did not contain %q", tc.title, tc.contentType, tc.wantLang, tc.wantLang+" · ")
		}
		if !strings.Contains(html, "<dt>language</dt><dd>"+tc.wantLang+"</dd>") {
			t.Errorf("title=%q contentType=%q: metadata panel missing language field %q", tc.title, tc.contentType, tc.wantLang)
		}
	}
}

// TestIntegrationCodeLanguageOverride asserts the `?lang=` query param
// overrides media_type/title detection (SPEC-0003 REQ "Code Viewer":
// "overridable").
func TestIntegrationCodeLanguageOverride(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifactWithTitle(t, srv.URL, "code", "joe", "print('hi')\n", "script.py", "text/plain")

	_, html := getHTML(t, srv.URL+"/"+id+"?lang=ruby")
	if !strings.Contains(html, "Ruby · ") {
		t.Errorf("?lang=ruby override: STATS line should show Ruby, html:\n%s", html)
	}
}

// TestIntegrationCodeLineCommentRoundTrip asserts a code_line comment posted
// through the shell's web composer (the `line` hidden field code.js's per-line
// 💬 button sets) persists and renders back in the panel with a "on line N"
// anchor context, and resolves to the correct line (SPEC-0003 REQ "Code
// Annotation Anchors").
func TestIntegrationCodeLineCommentRoundTrip(t *testing.T) {
	srv, client := sessionServer(t)
	id := createArtifactWithTitle(t, srv.URL, "code", "joe", codeDoc, "sample.go", "text/plain")

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
		"body": {"this string concat allocates"},
		"line": {"5"},
	}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post line comment = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, "this string concat allocates") {
		t.Error("line comment body should render in the panel")
	}
	if !strings.Contains(html, "on line 5") {
		t.Error("line comment should carry its line-number anchor context")
	}
}

// TestIntegrationCodeLineCommentRejectsNonPositiveLine asserts a malformed
// `line` field (zero, negative, non-numeric) is rejected as a validation
// error rather than silently anchoring to a bogus line.
func TestIntegrationCodeLineCommentRejectsNonPositiveLine(t *testing.T) {
	srv, client := sessionServer(t)
	id := createArtifactWithTitle(t, srv.URL, "code", "joe", codeDoc, "sample.go", "text/plain")

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	for _, bad := range []string{"0", "-1", "abc"} {
		resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
			"body": {"x"},
			"line": {bad},
		}, csrf)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("line=%q should be rejected, got 200", bad)
		}
		resp.Body.Close()
	}
}

// TestIntegrationCodeViewerDetectsRealWorldMediaType is the regression for a
// code artifact that rendered as unhighlighted "Plain Text" despite declaring
// text/x-go and naming a .go file in its title. chroma registers Go as
// text/x-gosrc, and the title was a filename PLUS prose, so BOTH detection
// paths missed and the body fell through to the plaintext lexer.
//
// Governing: SPEC-0003 REQ "Code Viewer"
func TestIntegrationCodeViewerDetectsRealWorldMediaType(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifactWithTitle(t, srv.URL, "code", "joe", codeDoc,
		"trajectory.go — the open category set", "text/x-go")

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}
	if strings.Contains(html, "Plain Text ·") {
		t.Error("a text/x-go body with a .go filename in its title must not render as Plain Text")
	}
	if !strings.Contains(html, "Go ·") {
		t.Error("expected the stats line to name the Go language")
	}
	// The line cell is white-space:pre, so a newline in the template renders as
	// a blank line — that double-spaced every code artifact.
	if strings.Contains(html, "<td class=\"code-line-cell\">\n") {
		t.Error("newline inside the pre line cell double-spaces the rendered source")
	}
}
