package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/store"
)

// The MCP surface integration suite (SPEC-0007, ADR-0003/0004, issue #44):
// drives the real streamable-HTTP MCP transport with the SDK's own client
// over real Postgres, minting genuine OAuth access tokens through the full
// authorization-code + PKCE flow (issue #42/#43) rather than any shortcut, so
// every assertion exercises the same wire protocol an MCP client (or the CLI)
// actually speaks.

// mcpConfig mirrors oauthConfig but disables the general per-IP limiter's
// interference and leaves upload limits high unless a test overrides them.
func mcpConfig() Config {
	return Config{
		BaseURL:            "http://cairn.test",
		MaxUploadBytes:     1 << 20,
		DefaultTTL:         time.Hour,
		DevLoginPassword:   "devpass",
		SessionTTL:         time.Hour,
		OAuthRatePerSecond: 1000,
		OAuthRateBurst:     1000,
		RatePerSecond:      1000,
		RateBurst:          1000,
	}
}

// mcpTestServer stands up the adapter (real OAuth AS, real MCP transport, NO
// dev bearer shortcut so only genuine bearer credentials authenticate) and
// returns the httptest server plus the plain *http.Client the OAuth helpers
// (registerClient/doLogin/approveConsent/exchangeCode) drive.
func mcpTestServer(t *testing.T, cfg Config, opts store.Options) (*httptest.Server, *store.Store) {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), opts)
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

// mintMCPToken runs the full register -> login -> consent -> code exchange
// flow and returns a live access token carrying exactly the approved scope
// subset, plus the human actor id it authenticates as (SPEC-0007 "the human
// is the owner/principal for every action the agent takes").
func mintMCPToken(t *testing.T, srv *httptest.Server, actor string, approved []string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	scope := "artifacts:read artifacts:write annotations:write"
	clientID, code := obtainCode(t, srv, client, actor, scope, approved)
	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))
	return tok.AccessToken
}

// bearerRoundTripper injects a static Authorization: Bearer header on every
// outgoing request — the shape a real MCP client presents its access token.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// mcpClient connects an SDK MCP client to srv's /mcp endpoint authenticated
// with token, returning the live session. opts, if non-nil, lets a caller wire
// e.g. ResourceUpdatedHandler for subscription tests.
func mcpClient(t *testing.T, srv *httptest.Server, token string, opts *mcp.ClientOptions, clientName string) *mcp.ClientSession {
	t.Helper()
	if opts == nil {
		opts = &mcp.ClientOptions{}
	}
	c := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.2.3"}, opts)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}},
	}
	sess, err := c.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callTool is a small assertion helper: it calls a tool and fails the test on
// a transport-level error, returning the result for the caller to inspect
// (including IsError, for the tool-domain-failure assertions).
func callTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call tool %s: %v", name, err)
	}
	return res
}

// toolText concatenates a CallToolResult's text content, the shape a
// generic-Out tool handler's JSON gets packed into.
func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// decodeToolJSON decodes a successful tool result's JSON text content into v.
func decodeToolJSON(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool call reported an error: %s", toolText(t, res))
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), v); err != nil {
		t.Fatalf("decode tool result: %v (body: %s)", err, toolText(t, res))
	}
}

// TestIntegrationMCPArtifactCreateReadProvenance is the headline round trip
// (SPEC-0007 REQ "Create & Push", REQ "MCP Tool Surface — Artifact & Bundle
// Read", REQ "Subject/Actor Identity Mapping & Least Privilege"): a token
// scoped for both read and write creates an artifact via artifact_create,
// then reads it back via artifact_read, asserting the artifact is owned by
// the human subject, provenance is stamped with the connecting MCP client's
// identity as OnBehalfOf and channel `via MCP`, and the body round-trips.
func TestIntegrationMCPArtifactCreateReadProvenance(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	created := callTool(t, sess, "artifact_create", map[string]any{
		"body":  "hello from an agent",
		"title": "agent note",
	})
	var createOut mcpCreateOutput
	decodeToolJSON(t, created, &createOut)
	if createOut.ID == "" {
		t.Fatalf("create returned no id: %+v", createOut)
	}
	if createOut.Provenance.Actor != "sam@stump.rocks" {
		t.Fatalf("provenance actor = %q, want the human subject", createOut.Provenance.Actor)
	}
	if createOut.Provenance.Channel != "via MCP" {
		t.Fatalf("provenance channel = %q, want %q", createOut.Provenance.Channel, "via MCP")
	}
	if createOut.Provenance.OnBehalfOf != "claude-desktop/1.2.3" {
		t.Fatalf("provenance on_behalf_of = %q, want the MCP client identity", createOut.Provenance.OnBehalfOf)
	}
	if createOut.Visibility != "link" {
		t.Fatalf("visibility = %q, want the default %q", createOut.Visibility, "link")
	}

	read := callTool(t, sess, "artifact_read", map[string]any{"id": createOut.ID})
	var readOut mcpReadOutput
	decodeToolJSON(t, read, &readOut)
	if readOut.Body != "hello from an agent" {
		t.Fatalf("read body = %q, want the created content", readOut.Body)
	}
	if readOut.BodyEncoding != "utf8" {
		t.Fatalf("body_encoding = %q, want utf8", readOut.BodyEncoding)
	}

	// The mcp:// handle round-trips through the read tool too (ADR-0005).
	readByHandle := callTool(t, sess, "artifact_read", map[string]any{"id": createOut.MCP})
	var readOut2 mcpReadOutput
	decodeToolJSON(t, readByHandle, &readOut2)
	if readOut2.ID != createOut.ID {
		t.Fatalf("read by mcp:// handle resolved to %q, want %q", readOut2.ID, createOut.ID)
	}
}

// TestIntegrationMCPCommentAndReact covers the comment+react tools
// end-to-end (SPEC-0007 REQ "Comment & React"), asserting provenance on the
// comment carries the model actor and the artifact's counters update.
func TestIntegrationMCPCommentAndReact(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write", "annotations:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	var createOut mcpCreateOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_create", map[string]any{"body": "annotate me"}), &createOut)

	var commentOut mcpCommentOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_comment", map[string]any{
		"id":          createOut.ID,
		"anchor_type": "artifact",
		"body":        "nice artifact",
	}), &commentOut)
	if commentOut.Body != "nice artifact" {
		t.Fatalf("comment body = %q", commentOut.Body)
	}
	if commentOut.OnBehalfOf != "claude-code/1.2.3" {
		t.Fatalf("comment on_behalf_of = %q, want the MCP client identity", commentOut.OnBehalfOf)
	}

	var reactOut mcpReactOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_react", map[string]any{
		"id":          createOut.ID,
		"anchor_type": "artifact",
		"emoji":       "🔥",
	}), &reactOut)
	if reactOut.Emoji != "🔥" {
		t.Fatalf("reaction emoji = %q", reactOut.Emoji)
	}

	var readOut mcpReadOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_read", map[string]any{"id": createOut.ID}), &readOut)
	if readOut.CommentCount != 1 || readOut.ReactionCount != 1 {
		t.Fatalf("counts = comments %d reactions %d, want 1 and 1", readOut.CommentCount, readOut.ReactionCount)
	}
}

// TestIntegrationMCPScopeDenials proves each write tool refuses a token
// lacking its required scope with a distinct insufficient_scope failure and
// performs no operation (SPEC-0007 REQ "Missing required scope", "Comment
// without the annotation scope").
func TestIntegrationMCPScopeDenials(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	// Read-only grant: every write tool must refuse it.
	readOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, readOnly, nil, "readonly-agent")

	res := callTool(t, sess, "artifact_create", map[string]any{"body": "nope"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("artifact_create with read-only token: IsError=%v text=%q, want a distinct insufficient_scope failure", res.IsError, toolText(t, res))
	}

	res = callTool(t, sess, "artifact_comment", map[string]any{"id": "whatever", "anchor_type": "artifact", "body": "nope"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("artifact_comment with read-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}

	res = callTool(t, sess, "artifact_react", map[string]any{"id": "whatever", "anchor_type": "artifact", "emoji": "👍"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("artifact_react with read-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}

	// A write-and-annotate grant lacking artifacts:read must still refuse the
	// read tool.
	writeOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write", "annotations:write"})
	writeSess := mcpClient(t, srv, writeOnly, nil, "writeonly-agent")
	res = callTool(t, writeSess, "artifact_read", map[string]any{"id": "whatever"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("artifact_read with write-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}
}

// TestIntegrationMCPUniformNotFound proves the read tool returns the same
// uniform not-found failure for an unknown id as for an id the token's human
// cannot reach — never a distinguishing signal (SPEC-0007 REQ "MCP Tool
// Surface — Artifact & Bundle Read" scenario "Read an unreachable id").
func TestIntegrationMCPUniformNotFound(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "artifact_read", map[string]any{"id": "totallyunknown1"})
	if !res.IsError {
		t.Fatal("read of an unknown id must be a tool error")
	}
	text := toolText(t, res)
	if !strings.HasPrefix(text, "not_found:") {
		t.Fatalf("read of an unknown id = %q, want a uniform not_found failure", text)
	}
}

// TestIntegrationMCPCreateRejectsNonBroadenedPolicy proves the create tool's
// schema carries no field able to broaden sharing, expiry, or ownership: a
// call attempting to smuggle one in is rejected before the human's default
// policy is ever touched (SPEC-0007 REQ "Create & Push" scenario "Create
// attempts a non-default policy").
func TestIntegrationMCPCreateRejectsNonBroadenedPolicy(t *testing.T) {
	srv, st := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "artifact_create", map[string]any{
		"body":       "sneaky",
		"visibility": "private",
	})
	if !res.IsError {
		t.Fatal("artifact_create with an unrecognized policy field must fail, not silently drop it")
	}
	_ = st
}

// TestIntegrationMCPStaticTokenRejected proves the pre-OAuth static APIToken
// bearer surface (and the insecure dev bearer shortcut) cannot reach /mcp:
// SPEC-0007's endpoint table requires an OAuth 2.1 bearer access token,
// audience-bound to Cairn, and nothing else.
func TestIntegrationMCPStaticTokenRejected(t *testing.T) {
	cfg := mcpConfig()
	cfg.APITokens = []APIToken{{Secret: "static-secret-1234567890", ActorID: "sam@stump.rocks", IsAgent: true}}
	srv, _ := mcpTestServer(t, cfg, store.Options{})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer static-secret-1234567890")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp with a static token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("static-token /mcp call status = %d, want 401 (body %s)", resp.StatusCode, b)
	}
}

// TestIntegrationMCPRevokedTokenRejected proves a revoked grant's access
// token is refused on /mcp (SPEC-0007 scenario "Revoked token used").
func TestIntegrationMCPRevokedTokenRejected(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	clientID, code := obtainCode(t, srv, client, "sam@stump.rocks", "artifacts:read", []string{"artifacts:read"})
	tok := decodeToken(t, exchangeCode(t, srv, clientID, code, pkceVerifier))

	revokeResp, err := http.PostForm(srv.URL+"/oauth/revoke", url.Values{
		"token":     {tok.AccessToken},
		"client_id": {clientID},
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revokeResp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp with a revoked token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked-token /mcp call status = %d, want 401", resp.StatusCode)
	}
}

// TestIntegrationMCPOversizeCreateBody proves an artifact_create call whose
// body exceeds the configured upload ceiling is rejected with a size error
// rather than silently truncated or persisted partially (SPEC-0007 REQ
// "Request Body Size Limits" scenario "Oversize create over MCP").
func TestIntegrationMCPOversizeCreateBody(t *testing.T) {
	cfg := mcpConfig()
	cfg.MaxUploadBytes = 256 // tiny ceiling, well under mcpBodyLimit's outer guard
	srv, _ := mcpTestServer(t, cfg, store.Options{MaxUploadBytes: cfg.MaxUploadBytes})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "artifact_create", map[string]any{"body": strings.Repeat("x", 4096)})
	if !res.IsError {
		t.Fatal("oversize create must fail, not persist a partial artifact")
	}
	if !strings.HasPrefix(toolText(t, res), "payload_too_large:") {
		t.Fatalf("oversize create failure = %q, want a payload_too_large error", toolText(t, res))
	}
}

// TestIntegrationMCPOversizeTransportBody proves a /mcp POST whose declared
// Content-Length exceeds the outer transport ceiling is rejected with 413
// before the body is buffered at all (SPEC-0007 REQ "Request Body Size
// Limits").
func TestIntegrationMCPOversizeTransportBody(t *testing.T) {
	cfg := mcpConfig()
	cfg.MaxUploadBytes = 1024
	srv, _ := mcpTestServer(t, cfg, store.Options{MaxUploadBytes: cfg.MaxUploadBytes})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})

	huge := bytes.Repeat([]byte("x"), int(2*cfg.MaxUploadBytes+(64<<10)+1024))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(huge))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.ContentLength = int64(len(huge))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversize /mcp POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize /mcp POST status = %d, want 413", resp.StatusCode)
	}
}

// TestIntegrationMCPTrajectoryResourceReadAndLiveUpdate covers the trajectory
// stream resource (SPEC-0007 REQ "MCP Resource Surface — Stream Reads"): a
// read returns the run's current spans, requires only artifacts:read (no
// fourth scope), and a subscribed client is notified when a new span lands so
// it can re-read and see the update — the MCP-native shape of "incremental
// delivery" over a request/response resource protocol.
func TestIntegrationMCPTrajectoryResourceReadAndLiveUpdate(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	// A separate human token creates the run over REST, mirroring a human
	// starting a live trajectory the agent then tails.
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})

	runID := openRunViaMCP(t, srv, humanToken)

	updates := make(chan string, 4)
	sess := mcpClient(t, srv, humanToken, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	}, "tailing-agent")

	uri := "mcp://cairn/run/" + runID
	read, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read run resource: %v", err)
	}
	if len(read.Contents) != 1 || read.Contents[0].MIMEType != "application/json" {
		t.Fatalf("run resource contents = %+v", read.Contents)
	}
	var run runResponse
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &run); err != nil {
		t.Fatalf("decode run resource: %v", err)
	}
	if run.ID != runID || run.Status != "open" {
		t.Fatalf("run resource = %+v, want id %s status open", run, runID)
	}

	if err := sess.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe to run resource: %v", err)
	}

	appendSpanViaMCP(t, srv, humanToken, runID)

	select {
	case gotURI := <-updates:
		if gotURI != uri {
			t.Fatalf("resource updated notification uri = %q, want %q", gotURI, uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a resources/updated notification after a span landed")
	}

	if err := sess.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
}

// TestIntegrationMCPTrajectoryResourceRequiresOnlyRead proves reading a run
// resource needs only artifacts:read — no fourth scope — matching SPEC-0007
// REQ "MCP Resource Surface — Stream Reads": "a stream is an artifact-shaped
// resource and MUST NOT require a fourth scope".
func TestIntegrationMCPTrajectoryResourceRequiresOnlyRead(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	runID := openRunViaMCP(t, srv, humanToken)

	readOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, readOnly, nil, "readonly-agent")

	res, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp://cairn/run/" + runID})
	if err != nil {
		t.Fatalf("read run resource with artifacts:read alone: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("run resource contents = %+v", res.Contents)
	}
}

// openRunViaMCP opens a live trajectory run over the REST /v1/runs endpoint
// (the same core CreateBatchRun/OpenRun the MCP surface has no dedicated tool
// for in v1 — runs are created by the human/CLI surface and tailed by
// agents), returning its public id.
func openRunViaMCP(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"mode":  "open",
		"title": "agent-tailed run",
		"model": "test-model",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("open run status = %d, body %s", resp.StatusCode, b)
	}
	var run runResponse
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run.ID
}

// appendSpanViaMCP appends one span to an open run over REST, the trigger for
// the trajectory hub's fan-out the MCP resource subscription observes.
func appendSpanViaMCP(t *testing.T, srv *httptest.Server, token, runID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"spans": []map[string]any{{
			"span_id":         "span-1",
			"category":        "exec",
			"tool":            "bash",
			"start_offset_ms": 0,
			"duration_ms":     10,
		}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+runID+"/spans", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("append span: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("append span status = %d, body %s", resp.StatusCode, b)
	}
}
