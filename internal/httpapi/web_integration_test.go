package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getHTML performs a GET and returns the status + body, following the shell's
// text/html responses.
func getHTML(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestIntegrationWebShellRendersArtifact is the story's headline acceptance: an
// artifact created through the core renders in the app shell at its canonical
// link-capability URL, with the header chrome, registry-resolved badge, and the
// panel present (SPEC-0001 REQ "Unified App Shell", REQ "Header Composition").
func TestIntegrationWebShellRendersArtifact(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Checkout Web Audit")

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}
	for _, frag := range []string{
		`role="banner"`,
		`class="url-control"`,
		`>MD<`,                              // registry badge
		`https://cairn.sh/` + id,            // canonical web URL
		`aria-label="Details and comments"`, // panel
		`>Provenance<`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("shell for %s missing %q", id, frag)
		}
	}
}

// TestIntegrationTrajectoryShellAtRunPrefix asserts the /run/{id} route renders
// a trajectory shell and that the two web routes each own exactly their
// canonical prefix: a trajectory is NOT reachable at the bare /{id}, and a
// bare-scheme artifact is NOT reachable at /run/{id}. Both wrong-path cases
// return the same uniform 404 as an unknown id (ADR-0007, SPEC-0001).
func TestIntegrationTrajectoryShellAtRunPrefix(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// Open an empty trajectory run through the core; it yields a trajectory
	// artifact whose canonical URL carries the /run/ prefix.
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "checkout-web-audit",
			Prompt: "Audit checkout.", Model: "claude-sonnet-4.6", StartedAt: fixedRunStart}),
		"application/json")
	run := decodeRun(t, resp)
	if run.ID == "" {
		t.Fatal("run create returned no id")
	}

	// Canonical: /run/{id} renders the trajectory shell.
	status, html := getHTML(t, srv.URL+"/run/"+run.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", run.ID, status)
	}
	if !strings.Contains(html, `>RUN<`) {
		t.Error("trajectory shell should carry the RUN badge (the #68 reader-facing name)")
	}
	if !strings.Contains(html, `mcp://cairn/run/`+run.ID) {
		t.Error("trajectory shell should surface the /run/ mcp handle")
	}

	// Wrong path: a trajectory at the bare /{id} is a uniform 404.
	if status, _ := getHTML(t, srv.URL+"/"+run.ID); status != http.StatusNotFound {
		t.Errorf("GET /%s (trajectory at bare path) = %d, want 404", run.ID, status)
	}

	// Wrong path: a bare-scheme artifact at /run/{id} is a uniform 404.
	fileID := createArtifact(t, srv.URL, "markdown", "joe", "# nope")
	if status, _ := getHTML(t, srv.URL+"/run/"+fileID); status != http.StatusNotFound {
		t.Errorf("GET /run/%s (bare artifact at run path) = %d, want 404", fileID, status)
	}
}

// TestIntegrationWebUniform404 asserts unknown/expired ids render the uniform
// 404 error page, leaking no signal (ADR-0007, SPEC-0001 REQ "Uniform 404").
func TestIntegrationWebUniform404(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	status, html := getHTML(t, srv.URL+"/zzzzzzzz")
	if status != http.StatusNotFound {
		t.Fatalf("GET /zzzzzzzz = %d, want 404", status)
	}
	if !strings.Contains(html, "not found or expired") {
		t.Error("uniform 404 page should show the leak-free not-found message")
	}
}

// TestIntegrationWebCommentsInPanel asserts a comment posted through the core
// renders read-only in the shell's panel (progressive read: comments are
// server-rendered, no JS required) (SPEC-0001 REQ "Progressive Enhancement").
func TestIntegrationWebCommentsInPanel(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Audit")

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "alice",
		jsonReader(t, commentRequest{AnchorType: "artifact", Body: "ship it"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post comment = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}
	if !strings.Contains(html, "ship it") {
		t.Error("panel should render the posted comment body")
	}
	if !strings.Contains(html, ">alice") {
		t.Error("panel should render the comment author")
	}
}

// TestIntegrationWebDownloadIsSniffProof asserts the web-surface download route
// (#12, supersedes upstream #2): an artifact body is served as a safe attachment
// — application/octet-stream + Content-Disposition: attachment + nosniff, under
// the shell CSP (not the /v1 API policy) — with bytes that round-trip exactly, so
// untrusted content (here an HTML/script payload) can never be sniffed into an
// executable type or rendered inline in Cairn's origin (SPEC-0001 REQ "Security
// Headers"). It also holds the canonical bare-scheme discipline: an unknown id
// and a bodyless/prefixed trajectory both yield the uniform 404 (ADR-0007).
func TestIntegrationWebDownloadIsSniffProof(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	body := "untrusted <script>alert(1)</script> bytes"
	id := createArtifact(t, srv.URL, "markdown", "joe", body)

	resp, err := http.Get(srv.URL + "/" + id + "/download")
	if err != nil {
		t.Fatalf("GET /%s/download: %v", id, err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("download Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("download Content-Disposition = %q, want attachment", cd)
	}
	if xcto := resp.Header.Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("download X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != webCSP {
		t.Errorf("download CSP = %q, want webCSP (web surface, not the /v1 policy)", csp)
	}
	if string(got) != body {
		t.Errorf("download body = %q, want %q", string(got), body)
	}

	// Unknown id → uniform 404.
	if status, _ := getHTML(t, srv.URL+"/zzzzzzzz/download"); status != http.StatusNotFound {
		t.Errorf("download unknown id = %d, want 404", status)
	}

	// A trajectory is bodyless and canonical at /run/{id}; a bare download 404s.
	rresp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "t", Prompt: "p",
			Model: "claude-sonnet-4.6", StartedAt: fixedRunStart}), "application/json")
	run := decodeRun(t, rresp)
	if status, _ := getHTML(t, srv.URL+"/"+run.ID+"/download"); status != http.StatusNotFound {
		t.Errorf("trajectory bare download = %d, want 404 (bodyless + prefixed)", status)
	}
}

// TestIntegrationShareDialogOnGenericShell asserts the Share dialog (#46,
// SPEC-0001 REQ "Share Affordance") renders on the generic (bare-scheme) shell:
// the dialog skeleton with the web link + mcp handle copy affordances, the
// access/expiry/provenance summary, and the owner-only note toggled correctly
// for an anonymous read versus the authenticated owner.
func TestIntegrationShareDialogOnGenericShell(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "joe", "# Checkout Web Audit")

	// Anonymous link read: the dialog is present but shows the owner-only note.
	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}
	for _, frag := range []string{
		`data-share-dialog`,
		`aria-label="Copy web link to clipboard"`,
		`aria-label="Copy MCP handle to clipboard"`,
		`https://cairn.sh/` + id,
		`mcp://cairn/` + id,
		`🔒 you &#43; anyone with link`,
		`⧗ expires`,
		"Only the owner can change this artifact's sharing policy.",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("generic shell share dialog (anon) missing %q", frag)
		}
	}

	// The authenticated owner sees the dialog with no owner-only note.
	resp := do(t, http.MethodGet, srv.URL+"/"+id, "joe", nil, "")
	ownerHTML := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s (owner) = %d, want 200", id, resp.StatusCode)
	}
	if strings.Contains(ownerHTML, "Only the owner can change") {
		t.Error("owner should not see the owner-only note in the share dialog")
	}

	// A different authenticated actor (non-owner) still sees the note.
	resp = do(t, http.MethodGet, srv.URL+"/"+id, "mallory", nil, "")
	otherHTML := readBody(t, resp)
	if !strings.Contains(otherHTML, "Only the owner can change") {
		t.Error("a non-owner viewer should see the owner-only note in the share dialog")
	}
}

// TestIntegrationShareDialogOnTrajectoryViewer asserts the Share dialog also
// renders on the trajectory viewer at /run/{id} — the two surfaces share one
// dialog partial (SPEC-0001 REQ "Share Affordance": "Works on both the generic
// shell and the trajectory viewer").
func TestIntegrationShareDialogOnTrajectoryViewer(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "open", Title: "checkout-web-audit",
			Prompt: "Audit checkout.", Model: "claude-sonnet-4.6", StartedAt: fixedRunStart}),
		"application/json")
	run := decodeRun(t, resp)

	status, html := getHTML(t, srv.URL+"/run/"+run.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s = %d, want 200", run.ID, status)
	}
	for _, frag := range []string{
		`data-share-dialog`,
		`aria-label="Copy web link to clipboard"`,
		`aria-label="Copy MCP handle to clipboard"`,
		`mcp://cairn/run/` + run.ID,
		"Only the owner can change this artifact's sharing policy.",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("trajectory viewer share dialog missing %q", frag)
		}
	}

	ownerResp := do(t, http.MethodGet, srv.URL+"/run/"+run.ID, "joe", nil, "")
	ownerHTML := readBody(t, ownerResp)
	if strings.Contains(ownerHTML, "Only the owner can change") {
		t.Error("run owner should not see the owner-only note in the share dialog")
	}
}

// TestIntegrationLandingAtRoot asserts the shell owns the web root: GET / is the
// on-brand landing, 200, not a bare 404.
func TestIntegrationLandingAtRoot(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	status, html := getHTML(t, srv.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", status)
	}
	if !strings.Contains(html, "AI-native artifact sharing") {
		t.Error("root should render the landing tagline")
	}
}

// TestIntegrationRESTProvenanceModel covers the REST half: a client reports its
// model through ?model= or X-Cairn-Model, the same query-or-header pairing
// `title` already uses, and it renders as provenance like any other surface.
func TestIntegrationRESTProvenanceModel(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	for name, tc := range map[string]struct{ url, header string }{
		"query param": {"/v1/artifacts?type=markdown&title=q&model=claude-opus-5", ""},
		"header":      {"/v1/artifacts?type=markdown&title=h", "claude-opus-5"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+tc.url, strings.NewReader("# body"))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer joe")
			req.Header.Set("Content-Type", "text/markdown")
			if tc.header != "" {
				req.Header.Set("X-Cairn-Model", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d, want 201", resp.StatusCode)
			}
			id := decodeArtifact(t, resp).ID

			status, html := getHTML(t, srv.URL+"/"+id)
			if status != http.StatusOK {
				t.Fatalf("GET /%s = %d, want 200", id, status)
			}
			if !strings.Contains(html, "claude-opus-5") {
				t.Error("the reported model must appear in the rendered provenance")
			}
		})
	}
}
