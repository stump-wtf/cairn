// Integration tests for the webhook inspector viewer (SPEC-0005 REQ
// "Inspector Viewer", ADR-0011, issue #86): the live-updating request list +
// per-request inspect view at the bare-scheme shell URL (`GET /{id}`), the
// reactions-only annotation asymmetry, and the empty state — end to end
// against a real Postgres store and object storage, exactly mirroring the
// pattern hook_ingress_integration_test.go/hooks_integration_test.go
// established for the management/ingest surfaces this viewer renders.
package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/webhook"
)

// TestIntegrationHookViewerEmptyState is the story's "awaiting its first
// request" acceptance (issue #86 checklist "Empty-state for a bin awaiting
// its first request"): a freshly created endpoint's shell renders the
// minimal empty state, its ingress URL, the endpoint-level reaction cluster,
// and NO request rows — with no comment affordance anywhere on the page
// (SPEC-0006 REQ "Webhook Reaction-Only Asymmetry").
func TestIntegrationHookViewerEmptyState(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	status, html := getHTML(t, srv.URL+"/"+created.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", created.ID, status)
	}

	for _, frag := range []string{
		`data-hook-viewer`,
		`data-hook-empty`,
		`No requests captured yet.`,
		created.IngressURL,            // the empty state points at the ingress address
		`>HK<`,                        // registry badge
		`data-anchor-type="artifact"`, // the whole-endpoint reaction cluster
		`hook.js`,                     // enhancement script wired in
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("hook viewer empty state missing %q\n---\n%s", frag, html)
		}
	}
	if strings.Contains(html, `data-hook-row`) {
		t.Error("fresh endpoint should render zero request rows")
	}
	if strings.Contains(html, `>Comments `) {
		t.Error("webhook shell must expose no comment affordance (SPEC-0006 Webhook Reaction-Only Asymmetry)")
	}
	if strings.Contains(html, `class="comment-composer"`) {
		t.Error("webhook shell must render no comment composer")
	}
}

// TestIntegrationHookViewerRendersCapturedRequest is the story's headline
// acceptance: a request captured over the open ingress appears in the
// inspector with its method/path/status/headers and a highlighted JSON body
// (SPEC-0005 REQ "Inspector Viewer": "Inspect a captured request"), rendered
// as inert text even though it carries an injection attempt (SPEC-0005
// Security REQ "No Payload Execution / Inert Capture").
func TestIntegrationHookViewerRendersCapturedRequest(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	payload := `{"event":"order.created","note":"<script>alert(1)</script>"}`
	// The ingress route is the fixed `ANY /h/{id}` pattern (no wildcard tail),
	// so every capture's Path is literally "/h/<id>"; only the query varies
	// per request (SPEC-0005 "method/path/query" metadata).
	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID)+"?source=stripe",
		strings.NewReader(payload), map[string]string{"Content-Type": "application/json", "X-Test": "1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingress capture = %d, want 200", resp.StatusCode)
	}

	status, html := getHTML(t, srv.URL+"/"+created.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", created.ID, status)
	}

	for _, frag := range []string{
		`data-seq="1"`,
		`data-status="200"`,
		`data-method="POST"`,                 // method badge, a text cue, never color-only
		`>POST<`,                             // method rendered as plain text
		`>200<`,                              // status rendered as plain text (not color-only)
		`data-class="2xx"`,                   // status-mix bucket
		created.ID + `?source=stripe</span>`, // captured path (fixed "/h/<id>", no wildcard tail) + query
		`chroma-chroma`,                      // reused code-viewer chroma highlighting
		`&#34;event&#34;`,                    // highlighted JSON key, HTML-escaped
		`order.created`,
		`x-test`,                             // sanitized headers are lowercased (internal/webhook/sanitize.go)
		`data-anchor-type="webhook_request"`, // per-request reaction cluster
		`data-span-id="1"`,                   // carries the seq (see hook_view.go doc comment)
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("hook viewer missing %q\n---\n%s", frag, html)
		}
	}
	// Inert capture: the injected script must never survive as live markup.
	for _, bad := range []string{"<script>alert", "onerror="} {
		if strings.Contains(html, bad) {
			t.Errorf("hook viewer leaked unescaped payload %q into the page", bad)
		}
	}
	if strings.Contains(html, `data-hook-empty`) {
		t.Error("a non-empty buffer must not render the empty state")
	}
	if strings.Contains(html, `>Comments `) || strings.Contains(html, "comment-anchor-btn") {
		t.Error("webhook shell must expose no comment affordance on a captured request")
	}
}

// TestIntegrationHookViewerReactOnRequest proves the reactions-only
// asymmetry end to end from the viewer's own annotation surface: reacting on
// a captured request's webhook_request anchor persists and renders on the
// next load (SPEC-0005 "React on a single request"), while a comment on the
// same request — or the whole endpoint — is refused (SPEC-0005 "Comment on a
// request refused").
func TestIntegrationHookViewerReactOnRequest(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID),
		strings.NewReader(`{"ok":true}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	// React 👀 on the captured request (seq 1) as an authenticated actor.
	reactResp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+created.ID+"/reactions", "joe",
		jsonReader(t, reactionRequest{
			AnchorType: "webhook_request",
			AnchorRef:  []byte(`{"request_id":"1"}`),
			Emoji:      "👀",
		}), "application/json")
	defer reactResp.Body.Close()
	if reactResp.StatusCode != http.StatusCreated {
		t.Fatalf("react on webhook_request = %d, want 201", reactResp.StatusCode)
	}

	// Also react on the whole endpoint (the `artifact` anchor webhook keeps,
	// SPEC-0006 "webhook reactions: artifact, webhook_request").
	endpointReact := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+created.ID+"/reactions", "joe",
		jsonReader(t, reactionRequest{AnchorType: "artifact", Emoji: "🔥"}), "application/json")
	defer endpointReact.Body.Close()
	if endpointReact.StatusCode != http.StatusCreated {
		t.Fatalf("react on artifact anchor = %d, want 201", endpointReact.StatusCode)
	}

	status, html := getHTML(t, srv.URL+"/"+created.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", created.ID, status)
	}
	if !strings.Contains(html, `data-emoji="👀"`) {
		t.Errorf("captured request should show the 👀 reaction pill\n---\n%s", html)
	}
	if !strings.Contains(html, `data-emoji="🔥"`) {
		t.Errorf("endpoint should show the 🔥 reaction pill\n---\n%s", html)
	}

	// A comment on the captured request is refused (write-time gate,
	// annotation.Validate), never silently accepted.
	commentResp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+created.ID+"/comments", "joe",
		jsonReader(t, commentRequest{AnchorType: "webhook_request", AnchorRef: []byte(`{"request_id":"1"}`), Body: "nope"}),
		"application/json")
	defer commentResp.Body.Close()
	if commentResp.StatusCode == http.StatusOK || commentResp.StatusCode == http.StatusCreated {
		t.Fatalf("comment on webhook_request = %d, want a rejection", commentResp.StatusCode)
	}

	// A comment on the whole endpoint is refused too — webhook grants no
	// comment anchor at all, not even whole-artifact (SPEC-0006).
	wholeArtifactComment := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+created.ID+"/comments", "joe",
		jsonReader(t, commentRequest{AnchorType: "artifact", Body: "nope"}), "application/json")
	defer wholeArtifactComment.Body.Close()
	if wholeArtifactComment.StatusCode == http.StatusOK || wholeArtifactComment.StatusCode == http.StatusCreated {
		t.Fatalf("comment on whole webhook artifact = %d, want a rejection", wholeArtifactComment.StatusCode)
	}
}

// TestIntegrationHookViewerLazySpilledBody proves an oversized captured body
// stays a lazy reference in the viewer — not inlined into the page — and
// fetches via the existing GET .../requests/{seq}/body route on expand
// (SPEC-0005 "Bodies MUST be fetched lazily when a request is expanded").
func TestIntegrationHookViewerLazySpilledBody(t *testing.T) {
	srv, hookSvc := hookTestServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	// Capture directly through the service with a body larger than the
	// default inline threshold, mirroring hooks_integration_test.go's own
	// direct-Capture pattern for scenarios the HTTP ingress path can't easily
	// drive (a deliberately oversized body here).
	big := strings.Repeat("x", 20000) // above webhook's 16 KiB default inline threshold
	if _, err := hookSvc.Capture(t.Context(), created.ID, webhook.CaptureInput{
		Method: "POST", Path: "/big", ContentType: "text/plain",
		Body: []byte(big), Status: webhook.DefaultResponseStatus,
	}); err != nil {
		t.Fatalf("direct capture: %v", err)
	}

	status, html := getHTML(t, srv.URL+"/"+created.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", created.ID, status)
	}
	if !strings.Contains(html, "data-hook-body-lazy") {
		t.Errorf("oversized body should render as a lazy reference\n---\n%s", html)
	}
	if !strings.Contains(html, "/v1/hooks/"+created.ID+"/requests/1/body") {
		t.Error("lazy body should point at the existing body route")
	}
	if strings.Contains(html, big) {
		t.Error("oversized body must not be inlined into the page")
	}
}
