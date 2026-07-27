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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/pat"
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

// mcpSpanList normalizes a span list to []map[string]any for tests that walk
// the tree without a typed Go struct. It accepts both shapes that occur: the
// typed []map[string]any of [mcpRunOutput].Spans, and the []any a nested span's
// "children" field decodes to (children live inside an untyped map, so they
// stay `any` however Spans itself is typed).
func mcpSpanList(v any) []map[string]any {
	switch arr := v.(type) {
	case []map[string]any:
		return arr
	case []any:
		out := make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
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

// TestIntegrationMCPPersonalAccessTokenAccepted proves a personal access token
// (cairn_pat_) authenticates on the MCP surface, not just /v1 — the fix for
// issue #51. A PAT is created in Settings explicitly "for my agents" (#74), and
// agents connect over MCP, so it must work here; its scopes gate every tool
// exactly as an OAuth access token's do, and provenance stamps the PAT owner.
func TestIntegrationMCPPersonalAccessTokenAccepted(t *testing.T) {
	srv, st := mcpTestServer(t, mcpConfig(), store.Options{})
	patSvc := pat.NewService(st.Pool())

	secret, _, err := patSvc.Create(context.Background(), "sam@stump.rocks", "crush-agent",
		[]string{oauth.ScopeArtifactsRead, oauth.ScopeArtifactsWrite}, true)
	if err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	// The MCP `initialize` handshake must succeed with the PAT as the bearer
	// (the exact step issue #51 saw return 401).
	sess := mcpClient(t, srv, secret, nil, "crush")
	created := callTool(t, sess, "artifact_create", map[string]any{"body": "made over MCP with a PAT", "share_type": "file"})
	if created.IsError {
		t.Fatalf("PAT-authenticated artifact_create failed: %s", toolText(t, created))
	}

	// Scope enforcement is unchanged: a read-only PAT is denied a write tool.
	roSecret, _, err := patSvc.Create(context.Background(), "sam@stump.rocks", "readonly",
		[]string{oauth.ScopeArtifactsRead}, true)
	if err != nil {
		t.Fatalf("create read-only PAT: %v", err)
	}
	roSess := mcpClient(t, srv, roSecret, nil, "crush-ro")
	denied := callTool(t, roSess, "artifact_create", map[string]any{"body": "x", "share_type": "file"})
	if !denied.IsError {
		t.Fatalf("read-only PAT should be denied artifact_create, got success")
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

// TestIntegrationMCPRunCreateRendersAtRunURL is the headline round trip for
// issue #65's run_create tool (SPEC-0007 REQ "Create & Push"): a run created
// over MCP with a parent/child span tree is readable via the trajectory-run
// MCP resource AND renders at /run/<id> with the right span tree — the same
// core trajectory.Service.CreateBatchRun the REST POST /v1/runs (mode
// "batch") handler calls, so both surfaces converge on the identical stored
// run. It also asserts provenance (human subject actor, channel `via MCP`,
// on_behalf_of the connecting MCP client identity) and derived stats.
func TestIntegrationMCPRunCreateRendersAtRunURL(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	created := callTool(t, sess, "run_create", map[string]any{
		"title":  "fix the flaky test",
		"prompt": "make CI green",
		"model":  "test-model",
		"spans": []map[string]any{
			{
				"span_id":         "root-1",
				"category":        "exec",
				"tool":            "bash",
				"name":            "run the suite",
				"args":            map[string]any{"cmd": "go test ./..."},
				"start_offset_ms": 0,
				"duration_ms":     500,
			},
			{
				"span_id":         "child-1",
				"parent_span_id":  "root-1",
				"category":        "read",
				"tool":            "read_file",
				"name":            "inspect the failure",
				"start_offset_ms": 100,
				"duration_ms":     50,
			},
		},
	})
	var runOut mcpRunOutput
	decodeToolJSON(t, created, &runOut)
	if runOut.ID == "" {
		t.Fatalf("run_create returned no id: %+v", runOut)
	}
	if runOut.Status != "closed" {
		t.Fatalf("run_create status = %q, want closed (batch runs are created closed)", runOut.Status)
	}
	if runOut.Stats.SpanCount != 2 || runOut.Stats.ToolCallCount != 2 {
		t.Fatalf("run stats = %+v, want span_count 2 tool_call_count 2", runOut.Stats)
	}
	if runOut.Provenance.Actor != "sam@stump.rocks" {
		t.Fatalf("provenance actor = %q, want the human subject", runOut.Provenance.Actor)
	}
	if runOut.Provenance.Channel != "via MCP" {
		t.Fatalf("provenance channel = %q, want %q", runOut.Provenance.Channel, "via MCP")
	}
	if runOut.Provenance.OnBehalfOf != "claude-code/1.2.3" {
		t.Fatalf("provenance on_behalf_of = %q, want the MCP client identity", runOut.Provenance.OnBehalfOf)
	}
	rootSpans := mcpSpanList(runOut.Spans)
	if len(rootSpans) != 1 || len(mcpSpanList(rootSpans[0]["children"])) != 1 {
		t.Fatalf("run spans = %+v, want one root with one child", runOut.Spans)
	}
	if args, ok := rootSpans[0]["args"].(map[string]any); !ok || args["cmd"] != "go test ./..." {
		t.Fatalf("root span args = %#v, want the embedded JSON object to round-trip, not a base64 string", rootSpans[0]["args"])
	}

	// The trajectory-run MCP resource reads back the identical span tree
	// (ADR-0003 parity between the tool's create response and the resource
	// read).
	uri := "mcp://cairn/run/" + runOut.ID
	read, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read run resource: %v", err)
	}
	var resourceRun runResponse
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &resourceRun); err != nil {
		t.Fatalf("decode run resource: %v", err)
	}
	if resourceRun.ID != runOut.ID || len(resourceRun.Spans) != 1 || len(resourceRun.Spans[0].Children) != 1 {
		t.Fatalf("run resource = %+v, want the same one-root-one-child tree", resourceRun)
	}

	// It also renders at /run/<id> with the right span tree: both the root
	// and child span markers the waterfall/stream templates emit
	// (data-spanjump="<span_id>") are present in the HTML.
	resp, err := http.Get(srv.URL + "/run/" + runOut.ID)
	if err != nil {
		t.Fatalf("GET /run/%s: %v", runOut.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /run/%s status = %d, want 200", runOut.ID, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /run/%s body: %v", runOut.ID, err)
	}
	html := string(body)
	if !strings.Contains(html, `data-spanjump="root-1"`) {
		t.Fatalf("/run/%s HTML has no waterfall row for root-1", runOut.ID)
	}
	if !strings.Contains(html, `data-spanjump="child-1"`) {
		t.Fatalf("/run/%s HTML has no waterfall row for child-1", runOut.ID)
	}
}

// TestIntegrationMCPRunAppendSpans covers the "optional but nice"
// run_append_spans tool (issue #65): a run opened live (over REST, mirroring
// a CLI-opened run an agent then appends to) accepts an appended span over
// MCP via the same trajectory.Service.AppendSpans the REST
// POST /v1/runs/{id}/spans handler calls, staying open; appending to a
// CLOSED run (one created via run_create) is refused with a conflict, never
// silently accepted.
func TestIntegrationMCPRunAppendSpans(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	runID := openRunViaMCP(t, srv, token)

	appended := callTool(t, sess, "run_append_spans", map[string]any{
		"id": runID,
		"spans": []map[string]any{{
			"span_id":         "span-a",
			"category":        "exec",
			"tool":            "bash",
			"start_offset_ms": 0,
			"duration_ms":     20,
		}},
	})
	var runOut mcpRunOutput
	decodeToolJSON(t, appended, &runOut)
	if runOut.ID != runID {
		t.Fatalf("run_append_spans id = %q, want %q", runOut.ID, runID)
	}
	if runOut.Status != "open" {
		t.Fatalf("run_append_spans status = %q, want open (append does not close)", runOut.Status)
	}
	appendedSpans := mcpSpanList(runOut.Spans)
	if len(appendedSpans) != 1 || appendedSpans[0]["span_id"] != "span-a" {
		t.Fatalf("run spans = %+v, want one span-a", runOut.Spans)
	}

	// Re-posting the same span_id is an idempotent no-op (SPEC-0004).
	reposted := callTool(t, sess, "run_append_spans", map[string]any{
		"id": runID,
		"spans": []map[string]any{{
			"span_id":         "span-a",
			"category":        "exec",
			"tool":            "bash",
			"start_offset_ms": 0,
			"duration_ms":     20,
		}},
	})
	var repostOut mcpRunOutput
	decodeToolJSON(t, reposted, &repostOut)
	if len(mcpSpanList(repostOut.Spans)) != 1 {
		t.Fatalf("re-posted span_id spans = %+v, want the idempotent no-op to still show exactly one span", repostOut.Spans)
	}

	// Appending to a run created (closed) via run_create must be refused.
	var closedOut mcpRunOutput
	decodeToolJSON(t, callTool(t, sess, "run_create", map[string]any{
		"title": "already closed",
		"spans": []map[string]any{{
			"span_id":         "only-span",
			"category":        "exec",
			"start_offset_ms": 0,
			"duration_ms":     5,
		}},
	}), &closedOut)

	res := callTool(t, sess, "run_append_spans", map[string]any{
		"id": closedOut.ID,
		"spans": []map[string]any{{
			"span_id":         "too-late",
			"category":        "exec",
			"start_offset_ms": 0,
			"duration_ms":     5,
		}},
	})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "conflict:") {
		t.Fatalf("append to a closed run: IsError=%v text=%q, want a conflict failure", res.IsError, toolText(t, res))
	}
}

// TestIntegrationMCPBundleCreate covers the bundle_create tool (issue #65):
// a bundle created over MCP with N named members is readable via
// artifact_read (listing members, then each member's body) exactly like a
// bundle created over the web/CLI multipart path — same core
// store.CreateBundle. It asserts provenance and the default link-visibility
// policy, matching artifact_create's contract.
func TestIntegrationMCPBundleCreate(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title": "patch bundle",
		"members": []map[string]any{
			{"name": "README.md", "body": "# patch notes", "media_type": "text/markdown"},
			{"name": "fix.diff", "body": "--- a\n+++ b\n"},
		},
	})
	var bundleOut mcpBundleCreateOutput
	decodeToolJSON(t, created, &bundleOut)
	if bundleOut.ID == "" {
		t.Fatalf("bundle_create returned no id: %+v", bundleOut)
	}
	if bundleOut.ShareType != "bundle" {
		t.Fatalf("bundle_create share_type = %q, want bundle", bundleOut.ShareType)
	}
	if bundleOut.Visibility != "link" {
		t.Fatalf("bundle_create visibility = %q, want the default %q", bundleOut.Visibility, "link")
	}
	if bundleOut.Provenance.Actor != "sam@stump.rocks" || bundleOut.Provenance.Channel != "via MCP" {
		t.Fatalf("bundle provenance = %+v, want actor sam@stump.rocks channel via MCP", bundleOut.Provenance)
	}
	if bundleOut.Provenance.OnBehalfOf != "claude-desktop/1.2.3" {
		t.Fatalf("bundle provenance on_behalf_of = %q, want the MCP client identity", bundleOut.Provenance.OnBehalfOf)
	}
	if len(bundleOut.Members) != 2 {
		t.Fatalf("bundle_create members = %+v, want 2", bundleOut.Members)
	}

	// The bundle's member list resolves via artifact_read (no path: lists
	// members; SPEC-0007 REQ "MCP Tool Surface — Artifact & Bundle Read").
	listed := callTool(t, sess, "artifact_read", map[string]any{"id": bundleOut.ID})
	var listOut mcpReadOutput
	decodeToolJSON(t, listed, &listOut)
	if len(listOut.Members) != 2 {
		t.Fatalf("artifact_read members = %+v, want 2", listOut.Members)
	}

	// Each member's body resolves too.
	readme := callTool(t, sess, "artifact_read", map[string]any{"id": bundleOut.ID, "path": "README.md"})
	var readmeOut mcpReadOutput
	decodeToolJSON(t, readme, &readmeOut)
	if readmeOut.Body != "# patch notes" {
		t.Fatalf("README.md body = %q, want the created content", readmeOut.Body)
	}
}

// TestIntegrationMCPRunAndBundleCreateRejectNonBroadenedPolicy proves
// run_create's and bundle_create's schemas, like artifact_create's, carry no
// field able to broaden sharing, expiry, or ownership: a call attempting to
// smuggle one in is rejected before the human's default policy is ever
// touched (SPEC-0007 REQ "Create & Push" scenario "Create attempts a
// non-default policy", issue #65 "Uniform not-broadening").
func TestIntegrationMCPRunAndBundleCreateRejectNonBroadenedPolicy(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "run_create", map[string]any{
		"title":      "sneaky run",
		"visibility": "private",
	})
	if !res.IsError {
		t.Fatal("run_create with an unrecognized policy field must fail, not silently drop it")
	}

	res = callTool(t, sess, "bundle_create", map[string]any{
		"members":    []map[string]any{{"name": "a.txt", "body": "hi"}},
		"visibility": "private",
	})
	if !res.IsError {
		t.Fatal("bundle_create with an unrecognized policy field must fail, not silently drop it")
	}
}

// TestIntegrationMCPRunAndBundleScopeDenials proves run_create,
// run_append_spans, and bundle_create each refuse a token lacking
// artifacts:write with a distinct insufficient_scope failure and perform no
// operation (SPEC-0007 REQ "Missing required scope", issue #65).
func TestIntegrationMCPRunAndBundleScopeDenials(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	readOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, readOnly, nil, "readonly-agent")

	res := callTool(t, sess, "run_create", map[string]any{"title": "nope"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("run_create with read-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}

	res = callTool(t, sess, "run_append_spans", map[string]any{"id": "whatever", "spans": []map[string]any{}})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("run_append_spans with read-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}

	res = callTool(t, sess, "bundle_create", map[string]any{
		"members": []map[string]any{{"name": "a.txt", "body": "hi"}},
	})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "insufficient_scope:") {
		t.Fatalf("bundle_create with read-only token: IsError=%v text=%q", res.IsError, toolText(t, res))
	}
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

// TestIntegrationMCPRunCapturePromptIsDiscoverable exercises the prompt over a
// real MCP session rather than by calling the handler: registration, the
// server's prompts capability, prompts/list, and prompts/get.
//
// The unit test covers the prompt's CONTENT; this one covers the part that can
// silently regress without any Go compile error — a prompt that is never
// advertised is exactly as useful to an agent as one that does not exist, which
// was the status quo before this change.
//
// Governing: ADR-0004 (MCP as a first-class surface), SPEC-0007 REQ "MCP Prompt
// Surface"
func TestIntegrationMCPRunCapturePromptIsDiscoverable(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "crush")

	listed, err := sess.ListPrompts(context.Background(), &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	var found *mcp.Prompt
	for _, p := range listed.Prompts {
		if p.Name == "run_capture" {
			found = p
		}
	}
	if found == nil {
		t.Fatalf("run_capture absent from prompts/list (got %d prompts)", len(listed.Prompts))
	}
	if found.Description == "" {
		t.Error("run_capture needs a description — it is how a client decides to surface it")
	}

	got, err := sess.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:      "run_capture",
		Arguments: map[string]string{"task": "capture this refactor"},
	})
	if err != nil {
		t.Fatalf("prompts/get: %v", err)
	}
	if len(got.Messages) == 0 {
		t.Fatal("prompts/get returned no messages")
	}
	text, ok := got.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("prompt content = %T, want *mcp.TextContent", got.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "capture this refactor") {
		t.Error("prompts/get should weave the task argument into the guidance")
	}
	// The guidance an agent most needs, end to end over the wire.
	for _, want := range []string{"output", "research", "reason", "sub-agent"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("run_capture guidance missing %q", want)
		}
	}
}

// TestIntegrationMCPSpanOutputIsPlainText pins the wire encoding of a span's
// output over MCP, which no test covered before and which was silently broken.
//
// Output was []byte, so the SDK inferred an ARRAY-OF-BYTES schema: the base64
// string REST accepts was rejected by schema validation, and the only encoding
// that worked was a JSON array of 0-255 integers — several times the size, for
// text the agent already holds as a string. An agent reading that schema would
// reasonably skip the field, which is a large part of why runs arrived with no
// output at all and rendered as empty rows.
//
// So this asserts the AGENT-FACING contract in both directions: the declared
// schema is a string, plain text round-trips through the viewer's data, and the
// text is stored verbatim rather than being base64-decoded on the way in.
//
// Governing: ADR-0004 (MCP as a first-class surface), SPEC-0007 REQ
// "Agent-Shaped Tool Schemas"
func TestIntegrationMCPSpanOutputIsPlainText(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "crush")

	// The declared schema an agent actually reads.
	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	// InputSchema is `any` on the wire, so walk it as decoded JSON rather than
	// reaching for an SDK schema type.
	var outSchema map[string]any
	for _, tl := range tools.Tools {
		if tl.Name != "run_create" {
			continue
		}
		root, _ := tl.InputSchema.(map[string]any)
		props, _ := root["properties"].(map[string]any)
		spansProp, _ := props["spans"].(map[string]any)
		items, _ := spansProp["items"].(map[string]any)
		spanProps, _ := items["properties"].(map[string]any)
		outSchema, _ = spanProps["output"].(map[string]any)
	}
	if outSchema == nil {
		t.Fatal("run_create span schema has no output property")
	}
	// `type` may be a bare string or a union list including "null".
	var types []string
	switch v := outSchema["type"].(type) {
	case string:
		types = []string{v}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				types = append(types, s)
			}
		}
	}
	if !slices.Contains(types, "string") {
		t.Errorf("output schema type = %v, want string — an array-of-bytes schema is unusable to an agent", types)
	}
	if slices.Contains(types, "array") {
		t.Error("output schema still declares an array; agents would have to send a list of byte integers")
	}

	// Plain text goes in verbatim and comes back out unchanged.
	const body = "thinking: the palette only maps thirteen names,\nso every other category fell to the neutral default."
	res := callTool(t, sess, "run_create", map[string]any{
		"title": "plain-text output", "model": "claude-opus-5",
		"spans": []map[string]any{{
			"span_id": "s1", "category": "research", "name": "a reasoning turn",
			"output": body, "start_offset_ms": 0, "duration_ms": 1200,
		}},
	})
	if res.IsError {
		t.Fatalf("run_create rejected plain-text output: %s", toolText(t, res))
	}
	var out mcpRunOutput
	decodeToolJSON(t, res, &out)

	if len(out.Spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(out.Spans))
	}
	got, _ := out.Spans[0]["output"].(string)
	if got != body {
		t.Errorf("output round-tripped as %q, want %q (stored verbatim, not re-encoded)", got, body)
	}
}

// TestIntegrationMCPSchemasHaveNoBooleanProperties pins every advertised tool
// schema to object-shaped properties.
//
// A Go field typed `any` infers as the JSON Schema boolean `true`. Draft
// 2020-12 permits a boolean as a `properties` value, but strict clients require
// a schema *object* there — and because tools/list is validated as a whole, a
// single such property rejects the entire list ("Invalid input at
// tools.N.inputSchema.properties.X"), taking every other tool down with it.
// Claude Code dropped all Cairn tools for exactly this reason: five `any`
// fields (anchor_ref on the comment/react input and both outputs, span args,
// and the run span tree) each emitted `true`.
//
// Governing: ADR-0004 (MCP as a first-class surface), SPEC-0007 REQ
// "Agent-Shaped Tool Schemas"
func TestIntegrationMCPSchemasHaveNoBooleanProperties(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks",
		[]string{oauth.ScopeArtifactsRead, oauth.ScopeArtifactsWrite, oauth.ScopeAnnotationsWrite})
	sess := mcpClient(t, srv, token, nil, "crush")

	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools advertised; the walk below would vacuously pass")
	}
	for _, tl := range tools.Tools {
		assertNoBooleanSchema(t, tl.Name+".inputSchema", tl.InputSchema)
		assertNoBooleanSchema(t, tl.Name+".outputSchema", tl.OutputSchema)
	}
}

// assertNoBooleanSchema walks a decoded JSON Schema and fails on any
// `properties` entry that is a bare boolean rather than a schema object,
// recursing through nested properties and array items.
func assertNoBooleanSchema(t *testing.T, path string, node any) {
	t.Helper()
	n, ok := node.(map[string]any)
	if !ok {
		return
	}
	if props, ok := n["properties"].(map[string]any); ok {
		for name, sub := range props {
			if b, isBool := sub.(bool); isBool {
				t.Errorf("%s.properties.%s is the boolean schema %v; a strict client rejects a non-object property and drops the whole tool list", path, name, b)
				continue
			}
			assertNoBooleanSchema(t, path+".properties."+name, sub)
		}
	}
	if items, ok := n["items"]; ok {
		assertNoBooleanSchema(t, path+".items", items)
	}
}

// TestIntegrationMCPProvenanceModel covers the one piece of provenance MCP
// cannot derive. `initialize` gives the harness its clientInfo (which becomes
// OnBehalfOf) and nothing on the wire names the model, so an agent must report
// it — before this, every non-trajectory artifact showed an actor and a harness
// with no way to say what produced it.
//
// Governing: ADR-0004 (provenance recorded server-side), SPEC-0007 REQ
// "Agent-Shaped Tool Schemas"
func TestIntegrationMCPProvenanceModel(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	// Declared on the schema, so an agent can discover it at all.
	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, want := range []string{"artifact_create", "bundle_create", "run_create"} {
		var has bool
		for _, tl := range tools.Tools {
			if tl.Name != want {
				continue
			}
			root, _ := tl.InputSchema.(map[string]any)
			props, _ := root["properties"].(map[string]any)
			_, has = props["model"]
		}
		if !has {
			t.Errorf("%s input schema has no model field; an agent cannot report its model", want)
		}
	}

	// Reported on create, it reaches the rendered page.
	res := callTool(t, sess, "artifact_create", map[string]any{
		"body": "# report", "title": "a report", "share_type": "markdown",
		"model": "claude-opus-5",
	})
	if res.IsError {
		t.Fatalf("artifact_create: %s", toolText(t, res))
	}
	var out mcpCreateOutput
	decodeToolJSON(t, res, &out)

	status, html := getHTML(t, srv.URL+"/"+out.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", out.ID, status)
	}
	if !strings.Contains(html, "claude-opus-5") {
		t.Error("the reported model must appear in the rendered provenance")
	}
	if !strings.Contains(html, "<dt>model</dt>") {
		t.Error("expected a model row in the provenance panel")
	}

	// Omitted, the row is absent rather than blank.
	res2 := callTool(t, sess, "artifact_create", map[string]any{
		"body": "# no model", "title": "no model", "share_type": "markdown",
	})
	var out2 mcpCreateOutput
	decodeToolJSON(t, res2, &out2)
	_, html2 := getHTML(t, srv.URL+"/"+out2.ID)
	if strings.Contains(html2, "<dt>model</dt>") {
		t.Error("an unreported model must omit the row, not render it empty")
	}
}
