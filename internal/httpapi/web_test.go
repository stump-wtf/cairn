package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/sharetype"
)

// newWebServer builds a Server with no store (so buildShellView exercises only
// the registry + template path, never the DB): enough to assert the shell's
// server-rendered HTML for any share type.
func newWebServer(t *testing.T) *Server {
	t.Helper()
	return New(nil, sharetype.Default(), nil, Config{BaseURL: "https://cairn.sh"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func fixtureArtifact(shareType artifact.ShareType, hasBody bool) *artifact.Artifact {
	a := &artifact.Artifact{
		PublicID:  "abc123",
		ShareType: shareType,
		Title:     "checkout-web-audit",
		Size:      1234,
		MediaType: "text/markdown",
		Provenance: artifact.Provenance{
			ActorID: "joe", Channel: artifact.ChannelMCP, CapturedAt: time.Now().Add(-2 * time.Hour),
		},
		Access:    artifact.AccessPolicy{OwnerID: "joe", Visibility: artifact.VisibilityLink},
		ExpiresAt: time.Now().Add(5 * 24 * time.Hour),
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	if hasBody {
		a.BodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}
	return a
}

func renderShellHTML(t *testing.T, s *Server, a *artifact.Artifact) string {
	t.Helper()
	vm := s.buildShellView(context.Background(), a)
	var sb strings.Builder
	if err := s.webTmpl.ExecuteTemplate(&sb, "shell", vm); err != nil {
		t.Fatalf("render shell (%s): %v", a.ShareType, err)
	}
	return sb.String()
}

// TestShellChromeIsIdenticalAcrossTypes is the ADR-0011 shell-consistency
// invariant: two artifacts of different share types render the SAME header
// structure (logo · badge · title · one URL control with copy + ◆ mcp · Share ·
// panel toggle) and the same collapsible panel, with only the badge/title/URL
// and body varying (SPEC-0001 REQ "Unified App Shell", REQ "Header Composition").
func TestShellChromeIsIdenticalAcrossTypes(t *testing.T) {
	s := newWebServer(t)

	// The chrome fragments every type must present, verbatim.
	chrome := []string{
		`role="banner"`,       // banner landmark (header)
		`class="url-control"`, // exactly one URL control
		`aria-label="Copy URL to clipboard"`,
		`◆ mcp`,                             // mcp affordance
		`class="share-btn"`,                 // Share button
		`aria-expanded`,                     // panel toggle exposes state
		`<main`,                             // main landmark
		`aria-label="Details and comments"`, // panel
		`>Provenance<`,                      // panel provenance section
		`>Comments `,                        // panel comments section
	}

	types := []artifact.ShareType{
		artifact.TypeFile,
		sharetype.KeyMarkdown,
		sharetype.KeyImage,
		sharetype.KeyWebhook,
		artifact.TypeTrajectory,
	}
	for _, tp := range types {
		html := renderShellHTML(t, s, fixtureArtifact(tp, tp != artifact.TypeTrajectory))
		for _, frag := range chrome {
			if !strings.Contains(html, frag) {
				t.Errorf("share type %q: shell missing required chrome fragment %q", tp, frag)
			}
		}
		// Exactly one URL control across every type.
		if n := strings.Count(html, `class="url-control"`); n != 1 {
			t.Errorf("share type %q: want exactly one URL control, got %d", tp, n)
		}
	}
}

// TestShellBadgeAndURLVaryByType asserts the two header data points that DO
// vary: the registry badge and the ADR-0005 URL scheme (a trajectory carries
// the /run/ sub-prefix on both the web link and the mcp handle).
func TestShellBadgeAndURLVaryByType(t *testing.T) {
	s := newWebServer(t)

	md := renderShellHTML(t, s, fixtureArtifact(sharetype.KeyMarkdown, true))
	if !strings.Contains(md, `https://cairn.sh/abc123`) {
		t.Error("markdown web URL should be the bare id path")
	}
	if !strings.Contains(md, `>MD<`) {
		t.Error("markdown badge should be MD")
	}

	trj := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	if !strings.Contains(trj, `https://cairn.sh/run/abc123`) {
		t.Error("trajectory web URL should carry the /run/ prefix")
	}
	if !strings.Contains(trj, `mcp://cairn/run/abc123`) {
		t.Error("trajectory mcp handle should carry the /run/ prefix")
	}
	if !strings.Contains(trj, `>TRJ<`) {
		t.Error("trajectory badge should be TRJ")
	}
}

// TestShellGenericBodyFloor asserts the total-resolution floor: a type with no
// registered BodyViewer renders the generic-file card (SPEC-0001 REQ
// "Type-Specific Body Slot"), with a download link when the body exists.
func TestShellGenericBodyFloor(t *testing.T) {
	s := newWebServer(t)

	withBody := renderShellHTML(t, s, fixtureArtifact(artifact.TypeFile, true))
	if !strings.Contains(withBody, `class="generic-card"`) {
		t.Error("a viewerless type should fall back to the generic card")
	}
	if !strings.Contains(withBody, `/v1/artifacts/abc123/body`) {
		t.Error("a bodied artifact should expose a download link")
	}

	bodyless := renderShellHTML(t, s, fixtureArtifact(artifact.TypeTrajectory, false))
	if strings.Contains(bodyless, `/v1/artifacts/abc123/body`) {
		t.Error("a bodyless artifact must not expose a download link")
	}
}

// TestShellEscapesUserContent asserts the shell HTML-escapes user-controlled
// strings (title) — the CSP forbids inline execution and the template layer
// output-encodes, so an injected tag cannot break out (SPEC-0001 Security).
func TestShellEscapesUserContent(t *testing.T) {
	s := newWebServer(t)
	a := fixtureArtifact(artifact.TypeFile, true)
	a.Title = `<script>alert(1)</script>`
	html := renderShellHTML(t, s, a)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("user title must be HTML-escaped, not rendered as a live tag")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("user title should appear escaped in the output")
	}
}

// TestWebRoutesCarryScopedCSP asserts the per-surface CSP scoping: HTML shell
// routes (landing, assets) get webCSP while the /v1 API keeps the strict
// default-src 'none' — neither loosens the other (SPEC-0001 REQ "Security
// Headers", CSP scoped by route).
func TestWebRoutesCarryScopedCSP(t *testing.T) {
	s := newWebServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Web landing: web CSP, permits self scripts/styles for HTMX+Alpine.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != webCSP {
		t.Errorf("landing CSP = %q, want webCSP", got)
	}
	// The shell renders untrusted user content (titles, comments); the vendored
	// Alpine CSP build + eval-free template markup mean the policy must never
	// carry 'unsafe-eval' or 'unsafe-inline' on this surface.
	for _, bad := range []string{"'unsafe-eval'", "'unsafe-inline'"} {
		if strings.Contains(webCSP, bad) {
			t.Errorf("webCSP must not contain %s (weakens the user-content surface): %q", bad, webCSP)
		}
	}

	// An embedded asset is served under the web CSP too.
	resp, err = http.Get(srv.URL + "/assets/app.css")
	if err != nil {
		t.Fatalf("GET /assets/app.css: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("asset status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "--page: #0A0B0D") {
		t.Error("app.css should be served from the embedded FS")
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != webCSP {
		t.Errorf("asset CSP = %q, want webCSP", got)
	}

	// The /v1 API keeps the strict policy.
	resp, err = http.Get(srv.URL + "/v1/artifacts/none")
	if err != nil {
		t.Fatalf("GET /v1/...: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
		t.Errorf("API CSP = %q, want the strict API policy (web CSP must not leak into /v1)", got)
	}
}

// TestLandingRenders asserts the root renders the on-brand landing, not chi's
// bare 404 (the shell owns the web root).
func TestLandingRenders(t *testing.T) {
	s := newWebServer(t)
	rec := httptest.NewRecorder()
	s.handleLanding(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("landing status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AI-native artifact sharing") {
		t.Error("landing should render the on-brand tagline")
	}
}

func TestHumanizeHelpers(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeSince(now.Add(-c.in)); got != c.want {
			t.Errorf("humanizeSince(-%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := humanizeBytes(1234); got != "1.2 kB" {
		t.Errorf("humanizeBytes(1234) = %q, want 1.2 kB", got)
	}
	if got := humanizeBytes(512); got != "512 B" {
		t.Errorf("humanizeBytes(512) = %q, want 512 B", got)
	}
	if got := humanizeUntil(now.Add(-time.Hour)); got != "expired" {
		t.Errorf("humanizeUntil(past) = %q, want expired", got)
	}
}
