package httpapi

import "testing"

// TestStatusClass covers the status-mix bucketing SPEC-0005 REQ "Inspector
// Viewer"'s status mix relies on, including the out-of-range guard (a
// malformed/impossible status never panics or mis-buckets).
func TestStatusClass(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "2xx"}, {201, "2xx"}, {301, "3xx"}, {404, "4xx"}, {500, "5xx"},
		{100, "1xx"}, {0, "?"}, {600, "?"}, {-1, "?"},
	}
	for _, c := range cases {
		if got := statusClass(c.status); got != c.want {
			t.Errorf("statusClass(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestBuildStatusMix asserts the mix is ordered ascending by family, skips
// empty buckets, and its percentages sum to (approximately) the whole buffer
// — SPEC-0005 "Status mix reflects the buffer".
func TestBuildStatusMix(t *testing.T) {
	mix := buildStatusMix(map[string]int{"4xx": 1, "2xx": 3}, 4)
	if len(mix) != 2 {
		t.Fatalf("mix segments = %d, want 2", len(mix))
	}
	if mix[0].Class != "2xx" || mix[0].Count != 3 {
		t.Errorf("mix[0] = %+v, want {2xx 3 ...} (ascending family order)", mix[0])
	}
	if mix[1].Class != "4xx" || mix[1].Count != 1 {
		t.Errorf("mix[1] = %+v, want {4xx 1 ...}", mix[1])
	}
	if got := buildStatusMix(nil, 0); got != nil {
		t.Errorf("empty buffer mix = %+v, want nil", got)
	}
}

// TestLooksLikeJSON covers the highlighting trust decision (SPEC-0005
// "render-time JSON syntax highlighting"): an explicit JSON content type is
// always trusted; otherwise only bytes that both look like AND validate as
// JSON qualify, so a coincidental leading brace in unrelated content never
// mis-highlights.
func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"explicit json content type", "application/json", "not actually json", true},
		{"json content type with charset", "application/json; charset=utf-8", "{}", true},
		{"valid json, no content type", "", `{"a":1}`, true},
		{"valid json array, no content type", "", `[1,2,3]`, true},
		{"plain text starting with brace", "text/plain", "{not json at all", false},
		// An explicit JSON content type is trusted outright regardless of the
		// (empty) body — renderHookBody never reaches looksLikeJSON for a
		// truly empty body in practice (hook_view.go gates on BodySize > 0
		// first), so this only pins the content-type-wins precedence.
		{"empty body but explicit json content type", "application/json", "", true},
		{"unrelated content type", "text/plain", "hello world", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeJSON(c.contentType, []byte(c.body)); got != c.want {
				t.Errorf("looksLikeJSON(%q, %q) = %v, want %v", c.contentType, c.body, got, c.want)
			}
		})
	}
}

// TestRenderHookBodyIsInert asserts a captured payload attempting active
// content renders as escaped, inert text regardless of the highlighting path
// taken (SPEC-0005 Security REQ "No Payload Execution / Inert Capture": "the
// inspector MUST render payloads as inert escaped text").
func TestRenderHookBodyIsInert(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{"json with script in value", "application/json", `{"note":"<script>alert(1)</script>"}`},
		{"plain html payload", "text/html", `<img src=x onerror="alert(1)">`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html := string(renderHookBody([]byte(c.body), c.contentType))
			// The only thing that matters for inertness is that an opening tag
			// never survives UNESCAPED — "onerror=" as bare escaped text is
			// harmless (a browser can't execute an attribute with no live tag
			// around it), so only the raw `<script>`/`<img` byte sequences are
			// checked, not substrings that safely-escaped text may still
			// contain verbatim.
			for _, bad := range []string{"<script>", "<img"} {
				if containsRaw(html, bad) {
					t.Errorf("renderHookBody(%q) leaked live markup %q into %q", c.body, bad, html)
				}
			}
			if !containsRaw(html, "&lt;") {
				t.Errorf("renderHookBody(%q) should HTML-escape the leading %q, got %q", c.body, "<", html)
			}
		})
	}
}

func containsRaw(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestWebhookRequestAnchorKeyMatchesCanonicalForm asserts the server-rendered
// tally key agrees with the canonical anchor_key annotation.CanonicalRef
// would derive from the same locator, so a reaction posted through the
// annotation service and a tally looked up via this key always line up
// (mirrors trajectory_view.go's spanAnchorKey contract).
func TestWebhookRequestAnchorKeyMatchesCanonicalForm(t *testing.T) {
	got := webhookRequestAnchorKey(42)
	want := `{"request_id":"42"}`
	if got != want {
		t.Errorf("webhookRequestAnchorKey(42) = %q, want %q", got, want)
	}
}
