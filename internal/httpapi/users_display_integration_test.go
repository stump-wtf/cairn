package httpapi

// Integration tests for the display-handle surfaces of audit A18 (SPEC-0023
// REQ "Users and Identities") beyond the artifact read covered in
// users_integration_test.go: the anonymous comment list, the run and hook
// reads, and their MCP resource twins. Each asserts that a reader who is not
// the actor sees a handle, and that the actor still sees their own email.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/store"
)

// The comment list is a link-capability read, so an anonymous reader reaches
// it. Against the unmasked handler it returned every commenter's email.
func TestIntegrationCommentListHidesCommenterEmail(t *testing.T) {
	srv := identityServer(t, newFakeIdP(t), nil)
	id := seedArtifact(t, srv, "sam@example.com")
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "pat@example.com",
		jsonReader(t, commentRequest{
			AnchorType: "text_selection",
			AnchorRef:  json.RawMessage(`{"start":2,"end":7,"quote":"owned"}`),
			Body:       "looks good",
		}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("comment = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	for reader, want := range map[string]string{
		"":                "pat",             // anonymous
		"sam@example.com": "pat",             // the artifact's owner, not the commenter
		"pat@example.com": "pat@example.com", // the commenter sees their own email
	} {
		list := decodeComments(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/comments", reader, nil, ""))
		if len(list.Comments) != 1 {
			t.Fatalf("reader %q: %d comments, want 1", reader, len(list.Comments))
		}
		if got := list.Comments[0].ActorID; got != want {
			t.Errorf("reader %q: comment actor = %q, want %q", reader, got, want)
		}
	}
}

// GET /v1/runs/{id} and GET /v1/hooks/{id} show the creator by handle to
// anyone else, and by email to the creator.
func TestIntegrationRunAndHookReadsHideCreatorEmail(t *testing.T) {
	srv := identityServer(t, newFakeIdP(t), nil)
	run := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "sam@example.com",
		jsonReader(t, runRequest{Mode: "open", Prompt: "p", StartedAt: fixedRunStart}), "application/json"))
	hook := decodeHook(t, do(t, http.MethodPost, srv.URL+"/v1/hooks", "sam@example.com",
		jsonReader(t, createHookRequest{Title: "masked"}), "application/json"))

	for reader, want := range map[string]string{
		"":                "sam",
		"pat@example.com": "sam",
		"sam@example.com": "sam@example.com",
	} {
		if got := decodeRun(t, do(t, http.MethodGet, srv.URL+"/v1/runs/"+run.ID, reader, nil, "")).Provenance.Actor; got != want {
			t.Errorf("reader %q: run actor = %q, want %q", reader, got, want)
		}
		if got := decodeHook(t, do(t, http.MethodGet, srv.URL+"/v1/hooks/"+hook.ID, reader, nil, "")).Provenance.Actor; got != want {
			t.Errorf("reader %q: hook actor = %q, want %q", reader, got, want)
		}
	}
}

// The mcp://cairn/run/<id> and mcp://cairn/hook/<id> resources render the
// same projection as the REST reads (ADR-0003 parity), so they mask the same
// way. Against the unmasked handlers another user's token read the email.
func TestIntegrationMCPRunAndHookResourcesHideCreatorEmail(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	sam := mintMCPToken(t, srv, "sam@example.com", []string{"artifacts:read", "artifacts:write"})
	pat := mintMCPToken(t, srv, "pat@example.com", []string{"artifacts:read"})
	runID := openRunViaMCP(t, srv, sam)
	hookID := createHookViaMCP(t, srv, sam).ID

	read := func(token, uri string) string {
		t.Helper()
		sess := mcpClient(t, srv, token, nil, "a18-reader")
		res, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		var v struct {
			Provenance provenanceView `json:"provenance"`
		}
		if err := json.Unmarshal([]byte(res.Contents[0].Text), &v); err != nil {
			t.Fatalf("decode %s: %v", uri, err)
		}
		return v.Provenance.Actor
	}
	for _, uri := range []string{"mcp://cairn/run/" + runID, "mcp://cairn/hook/" + hookID} {
		if got := read(pat, uri); got != "sam" {
			t.Errorf("another user's read of %s: actor = %q, want sam", uri, got)
		}
		if got := read(sam, uri); got != "sam@example.com" {
			t.Errorf("the creator's read of %s: actor = %q, want sam@example.com", uri, got)
		}
	}
}
