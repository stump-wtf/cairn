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
	if !strings.Contains(html, `>TRJ<`) {
		t.Error("trajectory shell should carry the TRJ badge")
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
