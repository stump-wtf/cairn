// Integration tests for the webhook open ingress (`ANY /h/{id}`, SPEC-0005,
// ADR-0010, issue #84): the single anonymous-write, unauthenticated route in
// Cairn. Every test here proves one of the Security Requirements SPEC-0005
// marks CRITICAL — inert capture, size caps before buffering, per-IP AND
// per-endpoint rate limiting, header hygiene, write-only capture, uniform
// 404 for unknown ids, and a fixed response no payload can steer.
package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/webhook"
)

// hookIngressDo issues a raw request against the ingress, letting the caller
// set arbitrary headers (Authorization/Cookie to prove redaction,
// X-Forwarded-For to simulate a distinct source IP for the per-IP-vs-
// per-endpoint rate-limit tests) that the do() helper's narrower signature
// cannot express.
func hookIngressDo(t *testing.T, method, url string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}

// createHookEndpoint provisions a webhook endpoint over the authenticated
// management API and returns its decoded view (SPEC-0005 story #83), the
// fixture every ingress test below points a "sender" at.
func createHookEndpoint(t *testing.T, srv string) hookResponse {
	t.Helper()
	created := decodeHook(t, do(t, http.MethodPost, srv+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{Title: "ingress test endpoint"}), "application/json"))
	// The response's own IngressURL field is built from cfg.BaseURL (a fixed
	// "https://cairn.sh" in these tests, see noRateLimit/testServer), never
	// the httptest server's ephemeral local address — SPEC-0005 "Endpoint
	// exposes both addresses" is about the SHAPE of the address the owner is
	// told, not this suite's transport. Tests below build the actually
	// reachable local address via ingressAddr(srv.URL, created.ID) instead,
	// exercising the identical /h/{id} route the real IngressURL would
	// resolve to at BaseURL.
	if want := "https://cairn.sh/h/" + created.ID; created.IngressURL != want {
		t.Fatalf("ingress URL = %q, want %q", created.IngressURL, want)
	}
	return created
}

// ingressAddr builds the actually-dialable local ingress address for a
// created endpoint against the running httptest server, mirroring the
// `/h/{id}` shape the real (BaseURL-rooted) IngressURL field reports.
func ingressAddr(base, id string) string {
	return base + "/h/" + id
}

// TestIntegrationHookIngressCapturesAnonymousRequest is the ingress happy
// path end to end (SPEC-0005 "an anonymous POST to the open route captures a
// request, visible via the management API"): no credential of any kind is
// presented, the ingress answers with the fixed benign response — never
// reflecting the payload — and the capture is immediately visible over the
// authenticated management API with the exact bytes, method, path, and
// query recorded, and Authorization/Cookie/hop-by-hop headers dropped
// (SPEC-0005 "Header Hygiene & Ephemerality as Containment").
func TestIntegrationHookIngressCapturesAnonymousRequest(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	payload := `{"event":"order.created","id":42}`
	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader(payload), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer super-secret-sender-token",
		"Cookie":        "session=leak-me-not",
		"X-Custom":      "kept",
	})
	defer resp.Body.Close()

	// Fixed, benign, inert response: 200, never anything derived from the
	// payload (SPEC-0005 REQ "Fixed Benign Response").
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingress status = %d, want 200", resp.StatusCode)
	}
	ackBody, _ := io.ReadAll(resp.Body)
	if string(ackBody) != hookIngressAck {
		t.Fatalf("ingress body = %q, want the fixed ack %q", ackBody, hookIngressAck)
	}
	if strings.Contains(string(ackBody), "order.created") || strings.Contains(string(ackBody), "42") {
		t.Fatal("ingress response reflects the posted payload — must be inert/fixed, not derived from input")
	}
	// The ingress carries the strict /v1-style CSP, never the HTMX/Alpine
	// webCSP (SPEC-0005 REQ "Security Headers").
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "default-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("ingress CSP = %q, want the strict API policy", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("ingress response missing nosniff header")
	}

	// The capture is visible over the authenticated management API — a
	// completely different, auth-gated surface — proving the two stories
	// wire into the same ring buffer (SPEC-0005 story #83 + #84).
	got := decodeHook(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID, "", nil, ""))
	if len(got.Requests) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(got.Requests))
	}
	captured := got.Requests[0]
	if captured.Method != http.MethodPost {
		t.Fatalf("captured method = %q, want POST", captured.Method)
	}
	if captured.Status != webhook.DefaultResponseStatus {
		t.Fatalf("captured status = %d, want the fixed %d", captured.Status, webhook.DefaultResponseStatus)
	}
	if string(captured.Body) != payload {
		t.Fatalf("captured body = %q, want %q (verbatim, SPEC-0005 REQ \"Request Capture and Storage\")", captured.Body, payload)
	}
	if _, ok := captured.Headers["authorization"]; ok {
		t.Fatal("Authorization header survived into the captured record — must be dropped on capture")
	}
	if _, ok := captured.Headers["cookie"]; ok {
		t.Fatal("Cookie header survived into the captured record — must be dropped on capture")
	}
	if vals, ok := captured.Headers["x-custom"]; !ok || len(vals) == 0 || vals[0] != "kept" {
		t.Fatalf("non-sensitive header x-custom = %v, want [kept] to survive sanitization", vals)
	}
}

// TestIntegrationHookIngressNoAuthRequired proves capturing never requires,
// and never leaks, any credential: an anonymous request with no
// Authorization header at all succeeds against a fresh endpoint no prior
// request has touched, and the response carries nothing an unauthenticated
// caller couldn't already know (SPEC-0005 "capturing does not require or
// leak auth").
func TestIntegrationHookIngressNoAuthRequired(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader("plain body"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated capture status = %d, want 200", resp.StatusCode)
	}
}

// TestIntegrationHookIngressWriteOnly proves a poster can never read back
// what it — or anyone else — captured: the ingress route only ever accepts
// (POST/PUT/etc into the ring buffer); there is no GET-and-see-my-own-post
// affordance on this route, and the fixed ack carries none of the buffer's
// contents (SPEC-0005 "Ingress grants no read").
func TestIntegrationHookIngressWriteOnly(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	// Seed one capture with an identifiable marker.
	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader("marker-XYZZY-body"), nil)
	resp.Body.Close()

	// A GET straight to the ingress (a poster's most natural "did it work?"
	// probe) still only gets the SAME fixed ack — never the buffer, never a
	// hint of what's inside.
	resp = hookIngressDo(t, http.MethodGet, ingressAddr(srv.URL, created.ID), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET ingress status = %d, want 200 (GET is captured like any other method)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "marker-XYZZY") {
		t.Fatal("ingress leaked prior captured content back to an anonymous caller")
	}
	if string(body) != hookIngressAck {
		t.Fatalf("GET ingress body = %q, want the fixed ack", body)
	}
}

// TestIntegrationHookIngressAnyMethod proves the ingress accepts every HTTP
// method, capturing each distinctly (SPEC-0005 "accepts inbound requests of
// any method").
func TestIntegrationHookIngressAnyMethod(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	methods := []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for _, m := range methods {
		resp := hookIngressDo(t, m, ingressAddr(srv.URL, created.ID), strings.NewReader("body-for-"+m), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s ingress status = %d, want 200", m, resp.StatusCode)
		}
		resp.Body.Close()
	}

	page := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests?limit=10", "", nil, ""))
	if len(page.Requests) != len(methods) {
		t.Fatalf("captured %d requests, want %d (one per method)", len(page.Requests), len(methods))
	}
	seen := map[string]bool{}
	for _, r := range page.Requests {
		seen[r.Method] = true
	}
	for _, m := range methods {
		if !seen[m] {
			t.Fatalf("method %s was never captured; seen=%v", m, seen)
		}
	}
}

// TestIntegrationHookIngressUnknownEndpoint404 proves an unguessed/unknown
// endpoint id is a uniform 404 on the ingress too, exactly like every other
// link-capability read in this adapter — probing leaks no signal about
// whether an id ever existed (SPEC-0005 REQ "Unguessable ID & No
// Enumeration").
func TestIntegrationHookIngressUnknownEndpoint404(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	resp := hookIngressDo(t, http.MethodPost, srv.URL+"/h/zzzzzzzz", strings.NewReader("x"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown endpoint ingress status = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}
}

// TestIntegrationHookIngressOversizeRejected proves a body over the hard cap
// is rejected with 413 BEFORE the full body is buffered, and — critically —
// that nothing is captured for a rejected request: no partial record, no
// partial blob (SPEC-0005 REQ "Request Body Size Limits").
func TestIntegrationHookIngressOversizeRejected(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	max := webhook.DefaultMaxBodyBytes
	oversized := bytes.Repeat([]byte("a"), int(max)+1)
	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), bytes.NewReader(oversized), map[string]string{
		"Content-Type": "application/octet-stream",
	})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize ingress status = %d, want 413", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "payload_too_large" {
		t.Fatalf("code = %q, want payload_too_large", env.Error.Code)
	}

	// Nothing was captured: the buffer stays empty.
	got := decodeHook(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID, "", nil, ""))
	if len(got.Requests) != 0 {
		t.Fatalf("buffer after oversize rejection = %d requests, want 0 (no partial capture)", len(got.Requests))
	}

	// A body AT the cap (not over it) is accepted.
	atCap := bytes.Repeat([]byte("b"), int(max))
	resp2 := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), bytes.NewReader(atCap), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("at-cap ingress status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
}

// TestIntegrationHookIngressRateLimitedPerIP proves the ingress carries its
// OWN dedicated per-source-IP limiter: flooding from one simulated source IP
// trips a 429 with Retry-After (and captures nothing on the throttled
// request), while a DIFFERENT source IP hitting the SAME endpoint is
// unaffected — the budget is keyed by sender, not shared with other callers
// (SPEC-0005 REQ "Rate Limiting": "per-source-IP").
func TestIntegrationHookIngressRateLimitedPerIP(t *testing.T) {
	// A tiny per-IP budget, a generous per-endpoint one, so only the IP
	// dimension can possibly trip.
	cfg := noRateLimit()
	cfg.HookIngressRatePerSecond = 0.001
	cfg.HookIngressRateBurst = 1
	cfg.HookEndpointRatePerSecond = 1000
	cfg.HookEndpointRateBurst = 1000
	srv := testServer(t, cfg, storeOpts())
	created := createHookEndpoint(t, srv.URL)

	ipA := map[string]string{"X-Forwarded-For": "203.0.113.10"}
	ipB := map[string]string{"X-Forwarded-For": "203.0.113.20"}

	// First request from IP A consumes its one token.
	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader("a1"), ipA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request from IP A = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Second request from IP A is throttled.
	resp = hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader("a2"), ipA)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request from IP A = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if env := decodeError(t, resp); env.Error.Code != "rate_limited" {
		t.Fatalf("code = %q, want rate_limited", env.Error.Code)
	}

	// IP B, hitting the SAME endpoint, has its own untouched budget.
	resp = hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID), strings.NewReader("b1"), ipB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request from IP B = %d, want 200 (per-IP budget is independent)", resp.StatusCode)
	}
	resp.Body.Close()

	// Only IP A's one successful capture (plus IP B's one) landed — the
	// throttled request captured nothing.
	page := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests?limit=10", "", nil, ""))
	if len(page.Requests) != 2 {
		t.Fatalf("captured requests = %d, want 2 (the throttled request must not have been captured)", len(page.Requests))
	}
}

// TestIntegrationHookIngressRateLimitedPerEndpoint proves the ingress ALSO
// carries a dedicated per-endpoint limiter, independent of the per-IP one:
// flooding ONE endpoint from a single IP trips 429s on that endpoint once
// its own budget is exhausted, while a DIFFERENT endpoint hit by the SAME
// IP immediately afterward is unaffected (SPEC-0005 REQ "Rate Limiting":
// "per-endpoint").
func TestIntegrationHookIngressRateLimitedPerEndpoint(t *testing.T) {
	// A tiny per-endpoint budget, a generous per-IP one, so only the
	// endpoint dimension can possibly trip.
	cfg := noRateLimit()
	cfg.HookEndpointRatePerSecond = 0.001
	cfg.HookEndpointRateBurst = 1
	cfg.HookIngressRatePerSecond = 1000
	cfg.HookIngressRateBurst = 1000
	srv := testServer(t, cfg, storeOpts())
	first := createHookEndpoint(t, srv.URL)
	second := createHookEndpoint(t, srv.URL)
	if first.ID == second.ID {
		t.Fatal("test fixture bug: expected two distinct endpoints")
	}

	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, first.ID), strings.NewReader("x1"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request to endpoint 1 = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, first.ID), strings.NewReader("x2"), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request to endpoint 1 = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}

	// The SAME source IP hitting a DIFFERENT endpoint is unaffected: the
	// per-endpoint budget for endpoint 2 is untouched.
	resp = hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, second.ID), strings.NewReader("y1"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request to endpoint 2 = %d, want 200 (per-endpoint budget is independent)", resp.StatusCode)
	}
	resp.Body.Close()

	page1 := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+first.ID+"/requests?limit=10", "", nil, ""))
	if len(page1.Requests) != 1 {
		t.Fatalf("endpoint 1 captured %d requests, want 1 (the throttled 2nd request must not land)", len(page1.Requests))
	}
	page2 := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+second.ID+"/requests?limit=10", "", nil, ""))
	if len(page2.Requests) != 1 {
		t.Fatalf("endpoint 2 captured %d requests, want 1", len(page2.Requests))
	}
}

// TestIntegrationHookIngressQueryAndPathCaptured proves the ingress records
// the inbound path and query string verbatim as metadata, queryable without
// ever touching the body (SPEC-0005 "Metadata queryable without reading the
// body").
func TestIntegrationHookIngressQueryAndPathCaptured(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	created := createHookEndpoint(t, srv.URL)

	resp := hookIngressDo(t, http.MethodPost, ingressAddr(srv.URL, created.ID)+"?ref=abc&n=1", strings.NewReader("{}"), nil)
	resp.Body.Close()

	page := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests", "", nil, ""))
	if len(page.Requests) != 1 {
		t.Fatalf("captured = %d, want 1", len(page.Requests))
	}
	got := page.Requests[0]
	if got.Query != "ref=abc&n=1" {
		t.Fatalf("captured query = %q, want %q", got.Query, "ref=abc&n=1")
	}
	if !strings.HasSuffix(got.Path, "/h/"+created.ID) {
		t.Fatalf("captured path = %q, want suffix /h/%s", got.Path, created.ID)
	}
}
