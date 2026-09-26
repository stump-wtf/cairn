package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/webhook"
)

// hookTestServer stands up the /v1 adapter exactly like testServer, but also
// returns a webhook.Service constructed over the SAME Postgres pool and object
// store the running server uses. This story deliberately mounts no public
// ingress endpoint (that lands in the next story per issue #83), so these
// tests call Capture directly — precisely how the eventual ingress handler
// will — to simulate inbound captures and then prove the management/read API
// surfaces them correctly.
func hookTestServer(t *testing.T, cfg Config, opts store.Options) (*httptest.Server, *webhook.Service) {
	t.Helper()
	cfg.DevInsecureBearerAuth = true
	pool := newTestPool(t)
	obj := objectstore.NewMemory()
	st := store.New(pool, obj, withScanner(opts))
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	hookSvc := webhook.NewService(pool, obj, webhook.Options{})
	return srv, hookSvc
}

func decodeHook(t *testing.T, resp *http.Response) hookResponse {
	t.Helper()
	defer resp.Body.Close()
	var h hookResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode hook: %v", err)
	}
	return h
}

func decodeHookRequests(t *testing.T, resp *http.Response) hookRequestsResponse {
	t.Helper()
	defer resp.Body.Close()
	var p hookRequestsResponse
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode hook requests: %v", err)
	}
	return p
}

// TestIntegrationHookCreateAndRead proves POST /v1/hooks provisions an
// endpoint exposing both addresses for the same endpoint — the bare web URL
// and the mcp://cairn/hook/<id> handle — and GET /v1/hooks/{id} reads it back
// with an empty buffer (SPEC-0005 "Endpoint exposes both addresses").
func TestIntegrationHookCreateAndRead(t *testing.T) {
	srv, _ := hookTestServer(t, noRateLimit(), storeOpts())

	resp := do(t, http.MethodPost, srv.URL+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{Title: "checkout webhook"}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create hook = %d, want 201", resp.StatusCode)
	}
	created := decodeHook(t, resp)
	if created.ID == "" {
		t.Fatal("created hook has no id")
	}
	if created.Title != "checkout webhook" {
		t.Fatalf("title = %q, want %q", created.Title, "checkout webhook")
	}
	if created.URL != "https://cairn.sh/"+created.ID {
		t.Fatalf("URL = %q, want bare https://cairn.sh/%s (SPEC-0005: web URL stays bare)", created.URL, created.ID)
	}
	if created.MCP != "mcp://cairn/hook/"+created.ID {
		t.Fatalf("MCP handle = %q, want mcp://cairn/hook/%s", created.MCP, created.ID)
	}
	if created.IngressURL != "https://cairn.sh/h/"+created.ID {
		t.Fatalf("ingress URL = %q, want https://cairn.sh/h/%s (SPEC-0005: endpoint exposes both addresses)", created.IngressURL, created.ID)
	}
	if created.RequestCap != webhook.DefaultRequestCap {
		t.Fatalf("request cap = %d, want default %d", created.RequestCap, webhook.DefaultRequestCap)
	}
	if len(created.Requests) != 0 {
		t.Fatalf("fresh endpoint requests = %v, want empty", created.Requests)
	}

	got := decodeHook(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID, "", nil, ""))
	if got.ID != created.ID || got.RequestCap != created.RequestCap {
		t.Fatalf("get hook diverges from create: %+v vs %+v", got, created)
	}

	// An explicit request_cap is honored.
	custom := decodeHook(t, do(t, http.MethodPost, srv.URL+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{RequestCap: 7}), "application/json"))
	if custom.RequestCap != 7 {
		t.Fatalf("custom request cap = %d, want 7", custom.RequestCap)
	}
}

// TestIntegrationHookAuthAndUniform404 covers the endpoint-security
// guarantees: unauthenticated creation is 401, and every read against an
// unknown endpoint id is an indistinguishable 404 (SPEC-0005 "Unauthenticated
// management call", "Unguessable ID & No Enumeration").
func TestIntegrationHookAuthAndUniform404(t *testing.T) {
	srv, _ := hookTestServer(t, noRateLimit(), storeOpts())

	resp := do(t, http.MethodPost, srv.URL+"/v1/hooks", "", jsonReader(t, createHookRequest{}), "application/json")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth create = %d, want 401", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", env.Error.Code)
	}

	for _, path := range []string{
		"/v1/hooks/zzzzzzzz",
		"/v1/hooks/zzzzzzzz/requests",
		"/v1/hooks/zzzzzzzz/requests/1",
		"/v1/hooks/zzzzzzzz/requests/1/body",
	} {
		resp := do(t, http.MethodGet, srv.URL+path, "", nil, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, resp.StatusCode)
		}
		if env := decodeError(t, resp); env.Error.Code != "not_found" {
			t.Fatalf("GET %s code = %q, want not_found", path, env.Error.Code)
		}
	}
}

// TestIntegrationHookCaptureVisibleOverManagementAPI proves a captured request
// — simulated here via Capture, the method the next story's public ingress
// will call — is queryable metadata-only (sanitized headers, the fixed
// status, no Authorization), paginates correctly, and its body is fetchable
// lazily and byte-exactly (SPEC-0005 "Request Capture and Storage", "Metadata
// queryable without reading the body", "Header Hygiene").
func TestIntegrationHookCaptureVisibleOverManagementAPI(t *testing.T) {
	srv, hookSvc := hookTestServer(t, noRateLimit(), storeOpts())
	created := decodeHook(t, do(t, http.MethodPost, srv.URL+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{}), "application/json"))

	ctx := t.Context()
	bodies := []string{`{"n":1}`, `{"n":2}`, `{"n":3}`}
	for i, body := range bodies {
		_, err := hookSvc.Capture(ctx, created.ID, webhook.CaptureInput{
			Method: "POST", Path: "/webhook", Query: fmt.Sprintf("n=%d", i+1),
			Headers:     map[string][]string{"Authorization": {"Bearer secret"}, "Content-Type": {"application/json"}},
			ContentType: "application/json",
			Body:        []byte(body),
			Status:      webhook.DefaultResponseStatus,
		})
		if err != nil {
			t.Fatalf("simulate capture %d: %v", i, err)
		}
	}

	// GET /v1/hooks/{id} returns the recent buffer, newest first, headers
	// sanitized.
	got := decodeHook(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID, "", nil, ""))
	if len(got.Requests) != 3 {
		t.Fatalf("buffer size = %d, want 3", len(got.Requests))
	}
	newest := got.Requests[0]
	if newest.Seq != 3 {
		t.Fatalf("newest seq = %d, want 3", newest.Seq)
	}
	if newest.Status != webhook.DefaultResponseStatus {
		t.Fatalf("status = %d, want the fixed %d", newest.Status, webhook.DefaultResponseStatus)
	}
	if _, ok := newest.Headers["authorization"]; ok {
		t.Fatal("Authorization must not survive sanitization into the response")
	}
	if string(newest.Body) != `{"n":3}` {
		t.Fatalf("inline body = %q, want %q", newest.Body, `{"n":3}`)
	}

	// Keyset pagination: limit=2 returns the newest two plus a cursor for the
	// remaining older one.
	page1 := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests?limit=2", "", nil, ""))
	if len(page1.Requests) != 2 || page1.Requests[0].Seq != 3 || page1.Requests[1].Seq != 2 {
		t.Fatalf("page1 = %+v, want seqs [3,2]", page1.Requests)
	}
	if page1.NextBefore != 2 {
		t.Fatalf("page1 next_before = %d, want 2", page1.NextBefore)
	}
	page2 := decodeHookRequests(t, do(t, http.MethodGet,
		srv.URL+"/v1/hooks/"+created.ID+"/requests?before=2", "", nil, ""))
	if len(page2.Requests) != 1 || page2.Requests[0].Seq != 1 {
		t.Fatalf("page2 = %+v, want seq [1]", page2.Requests)
	}
	if page2.NextBefore != 0 {
		t.Fatalf("page2 next_before = %d, want 0 (last page)", page2.NextBefore)
	}

	// Single-request detail.
	resp := do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests/1", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get request 1 = %d, want 200", resp.StatusCode)
	}
	var one requestView
	if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	resp.Body.Close()
	if one.Path != "/webhook" || one.Query != "n=1" {
		t.Fatalf("request 1 = %+v, want path=/webhook query=n=1", one)
	}

	// The body route streams the exact captured bytes with a non-executable
	// disposition (mirroring the trajectory span-output route).
	resp = do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests/1/body", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get body = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("body content-type = %q, want application/octet-stream", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("body response missing nosniff header")
	}
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(gotBody) != `{"n":1}` {
		t.Fatalf("body bytes = %q, want %q", gotBody, `{"n":1}`)
	}
}

// TestIntegrationHookRingBufferEvictionOverAPI proves the management API
// surfaces a capped ring buffer: a capture past the cap evicts the oldest, and
// its evicted seq becomes uniformly not-found (SPEC-0005 "Ring-Buffer
// Retention and Caps").
func TestIntegrationHookRingBufferEvictionOverAPI(t *testing.T) {
	srv, hookSvc := hookTestServer(t, noRateLimit(), storeOpts())
	created := decodeHook(t, do(t, http.MethodPost, srv.URL+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{RequestCap: 2}), "application/json"))

	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if _, err := hookSvc.Capture(ctx, created.ID, webhook.CaptureInput{Method: "GET", Status: 200}); err != nil {
			t.Fatalf("simulate capture %d: %v", i, err)
		}
	}

	page := decodeHookRequests(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests", "", nil, ""))
	if len(page.Requests) != 2 || page.Requests[0].Seq != 3 || page.Requests[1].Seq != 2 {
		t.Fatalf("buffer after overflow = %+v, want capped [3,2]", page.Requests)
	}

	resp := do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests/1", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("evicted request 1 = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("evicted request code = %q, want not_found", env.Error.Code)
	}
}

// TestIntegrationHookMalformedSeqRejected proves a non-numeric seq path
// segment is validation_failed rather than a misleading not-found.
func TestIntegrationHookMalformedSeqRejected(t *testing.T) {
	srv, _ := hookTestServer(t, noRateLimit(), storeOpts())
	created := decodeHook(t, do(t, http.MethodPost, srv.URL+"/v1/hooks", "joe",
		jsonReader(t, createHookRequest{}), "application/json"))

	resp := do(t, http.MethodGet, srv.URL+"/v1/hooks/"+created.ID+"/requests/not-a-number", "", nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed seq = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
}
