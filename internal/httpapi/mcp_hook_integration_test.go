// Integration tests for the webhook stream MCP resource
// (mcp://cairn/hook/<id>, SPEC-0007 REQ "MCP Resource Surface — Stream
// Reads", issue #85), mirroring TestIntegrationMCPTrajectoryResource* in
// mcp_integration_test.go exactly for the other live share type: a read
// returns the endpoint's current buffer, requires only artifacts:read (no
// fourth scope), and a subscribed client is notified when a new request is
// captured so it can re-read and see the update.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/store"
)

// createHookViaMCP provisions a webhook endpoint over the REST management API
// using an OAuth bearer token (mcpTestServer runs with no dev bearer
// shortcut, unlike hookTestServer/testServer), returning its public id —
// mirroring openRunViaMCP for the trajectory share type.
func createHookViaMCP(t *testing.T, srv *httptest.Server, token string) hookResponse {
	t.Helper()
	return decodeHook(t, do(t, "POST", srv.URL+"/v1/hooks", token, jsonReader(t, createHookRequest{Title: "mcp-tailed hook"}), "application/json"))
}

// TestIntegrationMCPHookResourceReadAndLiveUpdate is the webhook-stream
// analogue of TestIntegrationMCPTrajectoryResourceReadAndLiveUpdate: a read
// returns the endpoint's metadata and retained buffer, and a subscribed
// client receives a resources/updated notification when the anonymous
// ingress captures a new request (SPEC-0005 "Agent tails a webhook stream").
func TestIntegrationMCPHookResourceReadAndLiveUpdate(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})

	created := createHookViaMCP(t, srv, humanToken)

	updates := make(chan string, 4)
	sess := mcpClient(t, srv, humanToken, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	}, "hook-tailing-agent")

	uri := "mcp://cairn/hook/" + created.ID
	read, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read hook resource: %v", err)
	}
	if len(read.Contents) != 1 || read.Contents[0].MIMEType != "application/json" {
		t.Fatalf("hook resource contents = %+v", read.Contents)
	}
	var hook hookResponse
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &hook); err != nil {
		t.Fatalf("decode hook resource: %v", err)
	}
	if hook.ID != created.ID || len(hook.Requests) != 0 {
		t.Fatalf("hook resource = %+v, want id %s with an empty buffer", hook, created.ID)
	}

	if err := sess.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe to hook resource: %v", err)
	}

	// The anonymous open ingress captures a request — the SAME event source
	// the SSE endpoint tails (SPEC-0005 "Human and agent see the same
	// order").
	postCapture(t, ingressAddr(srv.URL, created.ID), "agent-visible")

	select {
	case gotURI := <-updates:
		if gotURI != uri {
			t.Fatalf("resource updated notification uri = %q, want %q", gotURI, uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a resources/updated notification after a capture landed")
	}

	// Re-reading after the notification shows the captured request — proving
	// the notification actually reflects new data, not just a bare ping.
	reread, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("re-read hook resource: %v", err)
	}
	var reHook hookResponse
	if err := json.Unmarshal([]byte(reread.Contents[0].Text), &reHook); err != nil {
		t.Fatalf("decode re-read hook resource: %v", err)
	}
	if len(reHook.Requests) != 1 || reHook.Requests[0].Query != "p=agent-visible" {
		t.Fatalf("re-read requests = %+v, want one request with query p=agent-visible", reHook.Requests)
	}

	if err := sess.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
}

// TestIntegrationMCPHookResourceRequiresOnlyRead proves reading a webhook
// stream resource needs only artifacts:read — no fourth scope — matching
// SPEC-0007 REQ "MCP Resource Surface — Stream Reads": "Reading a stream MUST
// require only artifacts:read".
func TestIntegrationMCPHookResourceRequiresOnlyRead(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	created := createHookViaMCP(t, srv, humanToken)

	readOnly := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, readOnly, nil, "hook-readonly-agent")

	res, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "mcp://cairn/hook/" + created.ID})
	if err != nil {
		t.Fatalf("read hook resource with artifacts:read alone: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("hook resource contents = %+v", res.Contents)
	}
}

// TestIntegrationMCPHookResourceNoWritePath proves the MCP surface exposes no
// tool that writes into a webhook stream — reads are the only affordance
// (SPEC-0007 "Scenario: Agent attempts to write a stream": "the server MUST
// reject it — streams are read-only to agents in v1"). There is no
// hook-specific write tool to call at all, so this asserts the tool list
// itself carries none.
func TestIntegrationMCPHookResourceNoWritePath(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, humanToken, nil, "hook-tool-lister")

	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "hook_create" || tool.Name == "hook_capture" || tool.Name == "hook_write" {
			t.Fatalf("found a webhook write tool %q — streams must be read-only to agents in v1", tool.Name)
		}
	}
}
