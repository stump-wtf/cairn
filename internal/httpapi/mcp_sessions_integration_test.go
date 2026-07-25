package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/store"
)

// The MCP agent-sessions integration suite (issue #76, SPEC-0007, ADR-0004):
// a real MCP `initialize` handshake over the real OAuth-authenticated
// transport records a session row with the connecting client's identity; a
// couple of tool calls increment its activity counters; GET
// /v1/mcp/sessions (owner-scoped, session-authenticated like the tokens
// routes) surfaces it to the Settings page; and ending it (DELETE) revokes
// the tied OAuth grant, cutting the agent off while leaving a sibling
// session untouched — the same "revoke anytime in settings" primitive every
// other connection uses.

// decodeJSONBody decodes and closes a 200 JSON response body, failing the
// test on a non-200 status or a decode error, mirroring createPAT's
// decode-or-fail shape (pat_integration_test.go).
func decodeJSONBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("response status = %d, body %s", resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// listMCPSessionsEventually polls GET /v1/mcp/sessions until it sees want
// sessions or a short deadline elapses. This is not papering over app-level
// asynchrony: mcp.Client.Connect returns as soon as its `initialized`
// notification's HTTP POST gets its 202 Accepted, which the streamable
// transport writes BEFORE the message is dequeued and dispatched to
// mcpInitializedHandler (mcp.go) — the SDK's own documented shape for
// notifications ("If the server accepts the input, return 202 Accepted with
// no body", no wait for processing). So there is a genuine, small,
// documented-as-acceptable window between "client thinks it's connected" and
// "the session row exists" (mcpsession.Service.Touch already treats a miss
// here as a silent no-op for exactly this reason) — a test asserting on the
// row immediately after Connect must tolerate that window the same way a
// real client's very first tool call would.
func listMCPSessionsEventually(t *testing.T, srvURL string, client *http.Client, want int) listMCPSessionsResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var listBody listMCPSessionsResponse
	for {
		resp := sessionJSON(t, srvURL, client, http.MethodGet, "/v1/mcp/sessions", nil)
		decodeJSONBody(t, resp, &listBody)
		if len(listBody.Sessions) >= want || time.Now().After(deadline) {
			return listBody
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestIntegrationMCPSessionRecordedAndActivityIncrements is the headline
// round trip: `initialize` records a session with the connecting client's
// name/version, and successful create/comment/react tool calls increment
// the right counters while a read only bumps tool_calls (SPEC-0007
// acceptance: "an MCP initialize + a couple tool calls create/increment a
// session row with the right client identity + actor").
func TestIntegrationMCPSessionRecordedAndActivityIncrements(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write", "annotations:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	var createOut mcpCreateOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_create", map[string]any{"body": "hello"}), &createOut)
	var readOut mcpReadOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_read", map[string]any{"id": createOut.ID}), &readOut)
	var commentOut mcpCommentOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_comment", map[string]any{
		"id": createOut.ID, "anchor_type": "artifact", "body": "nice",
	}), &commentOut)

	jar, _ := cookiejar.New(nil)
	humanClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, humanClient, "sam@stump.rocks", "devpass").Body.Close()

	resp := sessionJSON(t, srv.URL, humanClient, http.MethodGet, "/v1/mcp/sessions", nil)
	var listBody listMCPSessionsResponse
	decodeJSONBody(t, resp, &listBody)
	if len(listBody.Sessions) != 1 {
		t.Fatalf("sessions listed = %d, want 1", len(listBody.Sessions))
	}
	got := listBody.Sessions[0]
	if got.ClientName != "claude-code" || got.ClientVersion != "1.2.3" {
		t.Fatalf("client identity = %s/%s, want claude-code/1.2.3", got.ClientName, got.ClientVersion)
	}
	if got.ToolCalls != 3 {
		t.Fatalf("tool_calls = %d, want 3 (create + read + comment)", got.ToolCalls)
	}
	if got.ArtifactsCreated != 1 {
		t.Fatalf("artifacts_created = %d, want 1", got.ArtifactsCreated)
	}
	if got.AnnotationsPosted != 1 {
		t.Fatalf("annotations_posted = %d, want 1", got.AnnotationsPosted)
	}
	if got.Ended {
		t.Fatal("a freshly recorded session must not be reported ended")
	}
}

// TestIntegrationMCPSessionOwnerIsolation proves one human's
// GET /v1/mcp/sessions never surfaces another human's agent sessions
// (SPEC-0007 acceptance: "owner isolation").
func TestIntegrationMCPSessionOwnerIsolation(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})

	aliceToken := mintMCPToken(t, srv, "alice@stump.rocks", []string{"artifacts:read"})
	mcpClient(t, srv, aliceToken, nil, "alice-agent")
	bobToken := mintMCPToken(t, srv, "bob@stump.rocks", []string{"artifacts:read"})
	mcpClient(t, srv, bobToken, nil, "bob-agent")

	jar, _ := cookiejar.New(nil)
	bobClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, bobClient, "bob@stump.rocks", "devpass").Body.Close()

	listBody := listMCPSessionsEventually(t, srv.URL, bobClient, 1)
	if len(listBody.Sessions) != 1 {
		t.Fatalf("bob's session list = %d entries, want exactly his own", len(listBody.Sessions))
	}
	if listBody.Sessions[0].ClientName != "bob-agent" {
		t.Fatalf("bob's session client = %q, want bob-agent (never alice's)", listBody.Sessions[0].ClientName)
	}
}

// TestIntegrationMCPSessionEndRevokesGrantWithoutTouchingSibling proves
// ending a session (DELETE /v1/mcp/sessions/{id}) revokes its OAuth grant —
// the connected agent's very next call is refused — while a sibling agent's
// own session and grant keep working untouched (SPEC-0007 REQ "Token
// Revocation": "revoking a grant MUST invalidate that grant's access and
// refresh tokens without affecting the human's other connections").
func TestIntegrationMCPSessionEndRevokesGrantWithoutTouchingSibling(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})

	tokenA := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sessA := mcpClient(t, srv, tokenA, nil, "agent-a")
	tokenB := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sessB := mcpClient(t, srv, tokenB, nil, "agent-b")

	jar, _ := cookiejar.New(nil)
	humanClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, humanClient, "sam@stump.rocks", "devpass").Body.Close()

	listBody := listMCPSessionsEventually(t, srv.URL, humanClient, 2)
	if len(listBody.Sessions) != 2 {
		t.Fatalf("sessions listed = %d, want 2 (agent-a, agent-b)", len(listBody.Sessions))
	}
	var idA string
	for _, s := range listBody.Sessions {
		if s.ClientName == "agent-a" {
			idA = s.ID
		}
	}
	if idA == "" {
		t.Fatalf("could not find agent-a's session in %+v", listBody.Sessions)
	}

	endResp := sessionJSON(t, srv.URL, humanClient, http.MethodDelete, "/v1/mcp/sessions/"+idA, nil)
	endResp.Body.Close()
	if endResp.StatusCode != http.StatusNoContent {
		t.Fatalf("end session status = %d, want 204", endResp.StatusCode)
	}

	// Agent A's token is now revoked: the next tool call must fail. The
	// revoked bearer is refused at the /mcp transport's auth gate (401,
	// mirroring TestIntegrationMCPRevokedTokenRejected), which the SDK
	// client surfaces as a transport-level error rather than a
	// CallToolResult — there is no JSON-RPC response to unpack IsError from.
	if _, err := sessA.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "artifact_create", Arguments: map[string]any{"body": "should be refused"},
	}); err == nil {
		t.Fatal("agent-a's tool call should fail after its session was ended (grant revoked)")
	}

	// Agent B is untouched.
	var createOut mcpCreateOutput
	decodeToolJSON(t, callTool(t, sessB, "artifact_create", map[string]any{"body": "still works"}), &createOut)
	if createOut.ID == "" {
		t.Fatal("agent-b should still be able to create after agent-a's session was ended")
	}

	resp := sessionJSON(t, srv.URL, humanClient, http.MethodGet, "/v1/mcp/sessions", nil)
	decodeJSONBody(t, resp, &listBody)
	var endedA, endedB bool
	for _, s := range listBody.Sessions {
		switch s.ClientName {
		case "agent-a":
			endedA = s.Ended
		case "agent-b":
			endedB = s.Ended
		}
	}
	if !endedA {
		t.Error("agent-a's session should now report ended")
	}
	if endedB {
		t.Error("agent-b's session should NOT report ended")
	}
}

// TestIntegrationMCPSessionEndCrossOwnerIsNotFound proves a human cannot end
// another owner's session by id — the same owner-scoped-uniform-404
// discipline handleRevokeToken uses.
func TestIntegrationMCPSessionEndCrossOwnerIsNotFound(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})

	aliceToken := mintMCPToken(t, srv, "alice@stump.rocks", []string{"artifacts:read"})
	mcpClient(t, srv, aliceToken, nil, "alice-agent")

	jarAlice, _ := cookiejar.New(nil)
	aliceClient := &http.Client{Jar: jarAlice, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, aliceClient, "alice@stump.rocks", "devpass").Body.Close()
	listBody := listMCPSessionsEventually(t, srv.URL, aliceClient, 1)
	if len(listBody.Sessions) != 1 {
		t.Fatalf("alice's sessions = %d, want 1", len(listBody.Sessions))
	}
	aliceSessionID := listBody.Sessions[0].ID

	jarBob, _ := cookiejar.New(nil)
	bobClient := &http.Client{Jar: jarBob, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, bobClient, "bob@stump.rocks", "devpass").Body.Close()

	endResp := sessionJSON(t, srv.URL, bobClient, http.MethodDelete, "/v1/mcp/sessions/"+aliceSessionID, nil)
	endResp.Body.Close()
	if endResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner end session status = %d, want 404", endResp.StatusCode)
	}
}
