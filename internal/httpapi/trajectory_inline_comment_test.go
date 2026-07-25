package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestIntegrationTrajectoryInlineCommentButtonMarkup pins the server-rendered
// markup the inline composer (#58) depends on: the per-span "💬 comment"
// button (data-comment-span/data-comment-name, trajectory.js openCommentComposer)
// renders for EVERY viewer, signed in or anonymous, so a signed-out click has
// something to wire a /login?next= redirect onto rather than the button being
// silently absent (consistent with the reaction affordance fixed in #41). The
// RUN panel's own whole-run composer, by contrast, only renders once signed
// in, and no longer carries a span_id hidden field — the per-span anchor now
// travels entirely through the inline composer's own POST, not this form.
func TestIntegrationTrajectoryInlineCommentButtonMarkup(t *testing.T) {
	srv, client := sessionServer(t)
	runID := seedAuditRun(t, srv.URL)

	// Anonymous: the span comment buttons are present (wired client-side to
	// redirect to /login?next=), but the RUN panel composer form is not.
	status, anon := getHTML(t, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s (anon) = %d, want 200", runID, status)
	}
	if !strings.Contains(anon, `data-comment-span="s1"`) {
		t.Error("anonymous view should still render the per-span comment button (data-comment-span)")
	}
	if strings.Contains(anon, `data-comment-form`) {
		t.Error("anonymous view should not render the RUN panel composer form")
	}
	if !strings.Contains(anon, "comments-signedout") {
		t.Error("anonymous view should render the sign-in-to-comment prompt")
	}

	// Authenticated: both the per-span buttons AND the RUN panel composer
	// render, but the composer no longer carries a span_id hidden field (the
	// per-span anchor is set by the inline composer's own POST, #58).
	resp := doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	status, authed := getHTMLClient(t, client, srv.URL+"/run/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /run/%s (authed) = %d, want 200", runID, status)
	}
	if !strings.Contains(authed, `data-comment-span="s1"`) {
		t.Error("authenticated view should render the per-span comment button")
	}
	if !strings.Contains(authed, `data-comment-form`) {
		t.Error("authenticated view should render the RUN panel composer form")
	}
	if strings.Contains(authed, `name="span_id"`) {
		t.Error("the RUN panel composer should no longer carry a span_id hidden field (#58: per-span anchors go through the inline composer)")
	}
}

// TestIntegrationTrajectoryInlineCommentPostReturnsAppendableFragment asserts
// the endpoint the inline composer's fetch() posts to (the same
// `POST /{id}/comments` the RUN panel form uses) returns a bare comment
// partial — not a full page — so trajectory.js can append it straight into
// #comment-list the same way HTMX's own hx-swap="beforeend" would (#58).
func TestIntegrationTrajectoryInlineCommentPostReturnsAppendableFragment(t *testing.T) {
	srv, client := sessionServer(t)
	runID := seedAuditRun(t, srv.URL)

	resp := doLogin(t, srv, client, "joe", "devpass")
	resp.Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	if csrf == "" {
		t.Fatal("login did not seed a CSRF cookie")
	}

	resp = postForm(t, client, srv.URL+"/"+runID+"/comments",
		url.Values{"body": {"inline composer round-trip"}, "span_id": {"s2"}}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inline comment post = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if strings.Contains(body, "<html") || strings.Contains(body, "<!doctype") {
		t.Error("comment post response should be a bare partial, not a full page, so it can be appended client-side")
	}
	if !strings.Contains(body, "inline composer round-trip") {
		t.Errorf("comment partial should carry the posted body, got %q", body)
	}
	if !strings.Contains(body, "on span s2") {
		t.Errorf("comment partial should carry the span anchor context, got %q", body)
	}

	// It also persists into the panel thread on a fresh load, so the count and
	// list trajectory.js updates client-side agree with the server's own view.
	_, html := getHTMLClient(t, client, srv.URL+"/run/"+runID)
	if !strings.Contains(html, "inline composer round-trip") {
		t.Error("panel should render the inline-composer comment on reload")
	}
}
