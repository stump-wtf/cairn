package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: SPEC-0024 REQ "Declaring Relations on Create", REQ "Trace
// Context on Create"; ADR-0030 (SPEC-0024 slice 2: the MCP create surface).

// restGetArtifact performs a bearer-authenticated REST read and decodes the
// full read response (relations, referrers and trace included).
func restGetArtifact(t *testing.T, srv *httptest.Server, token, id string) artifactReadResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/artifacts/"+id, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/artifacts/%s = %d, want 200", id, resp.StatusCode)
	}
	var out artifactReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	return out
}

func TestIntegrationMCPRelationsAndTraceOnCreate(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	target := callTool(t, sess, "artifact_create", map[string]any{
		"body":  "the thing replied to",
		"title": "the target",
	})
	var targetOut mcpCreateOutput
	decodeToolJSON(t, target, &targetOut)

	created := callTool(t, sess, "artifact_create", map[string]any{
		"body":  "a reply written over MCP",
		"title": "the reply",
		"relations": []any{
			map[string]any{"kind": "reply_to", "id": targetOut.ID},
			map[string]any{"kind": "follows", "id": targetOut.ID},
		},
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})
	var createdOut mcpCreateOutput
	decodeToolJSON(t, created, &createdOut)

	read := restGetArtifact(t, srv, token, createdOut.ID)
	if len(read.Relations) != 2 {
		t.Fatalf("read relations = %+v, want reply_to + follows", read.Relations)
	}
	if read.Relations[0].Kind != artifact.RelationReplyTo || read.Relations[0].ID != targetOut.ID {
		t.Fatalf("first relation = %+v, want reply_to %s", read.Relations[0], targetOut.ID)
	}
	if !read.Relations[0].Available || read.Relations[0].Title != "the target" {
		t.Fatalf("first relation = %+v, want an available target with its title", read.Relations[0])
	}
	if read.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %q, want the traceparent's trace id", read.TraceID)
	}

	// The referrers list names each replying artifact once, under the first
	// kind it declared (SPEC-0024 REQ "Relations on Read").
	targetRead := restGetArtifact(t, srv, token, targetOut.ID)
	if len(targetRead.Referenced) != 1 {
		t.Fatalf("target referenced_by = %+v, want the reply listed once", targetRead.Referenced)
	}
	ref := targetRead.Referenced[0]
	if ref.Kind != artifact.RelationReplyTo || ref.ID != createdOut.ID ||
		!ref.Available || ref.Title != "the reply" {
		t.Fatalf("referrer = %+v, want the reply under its first declared kind", ref)
	}
}

func TestIntegrationMCPBundleCreateRelationsAndTrace(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	target := callTool(t, sess, "artifact_create", map[string]any{"body": "derived from this"})
	var targetOut mcpCreateOutput
	decodeToolJSON(t, target, &targetOut)

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title":   "a derived bundle",
		"members": []any{map[string]any{"name": "a.txt", "body": "one"}},
		"relations": []any{
			map[string]any{"kind": "derived_from", "id": targetOut.ID},
		},
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})
	var createdOut mcpBundleCreateOutput
	decodeToolJSON(t, created, &createdOut)

	read := restGetArtifact(t, srv, token, createdOut.ID)
	if len(read.Relations) != 1 || read.Relations[0].Kind != artifact.RelationDerivedFrom {
		t.Fatalf("bundle relations = %+v, want one derived_from", read.Relations)
	}
	if read.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("bundle trace_id = %q, want the traceparent's trace id", read.TraceID)
	}
}

func TestIntegrationMCPRelationRejections(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	t.Run("unknown kind names the entry", func(t *testing.T) {
		res := callTool(t, sess, "artifact_create", map[string]any{
			"body":      "x",
			"relations": []any{map[string]any{"kind": "replies", "id": "zzzzzzzz"}},
		})
		code, vs := toolViolations(t, res)
		if code != errs.CodeValidation || len(vs) != 1 || vs[0].Field != "relations[0].kind" {
			t.Fatalf("result = %s %+v, want one violation on relations[0].kind", code, vs)
		}
	})

	t.Run("unreadable target is the one uniform error", func(t *testing.T) {
		res := callTool(t, sess, "artifact_create", map[string]any{
			"body":      "x",
			"relations": []any{map[string]any{"kind": "reply_to", "id": "zzzzzzzzzzzzzzzzzzzzzz"}},
		})
		if !res.IsError {
			t.Fatal("create with an unknown target must fail")
		}
		// The sentinel carries no violation of its own, so it renders as the
		// generic validation error: unknown, expired and unreadable targets are
		// never told apart (SPEC-0024 REQ "Relation Targets Must Be Readable by
		// the Creator").
		code, vs := toolViolations(t, res)
		if code != errs.CodeValidation {
			t.Fatalf("code = %q, want the validation code the sentinel renders as", code)
		}
		for _, v := range vs {
			if !isGeneric(v) {
				t.Fatalf("violations = %+v, want nothing that names which target failed", vs)
			}
		}
	})

	t.Run("cap is reported once on relations", func(t *testing.T) {
		rels := make([]any, 0, artifact.MaxRelations+1)
		for i := 0; i <= artifact.MaxRelations; i++ {
			rels = append(rels, map[string]any{"kind": "follows", "id": fmt.Sprintf("a%022d", i)})
		}
		res := callTool(t, sess, "artifact_create", map[string]any{"body": "x", "relations": rels})
		code, vs := toolViolations(t, res)
		if code != errs.CodeValidation || len(vs) != 1 || vs[0].Field != "relations" {
			t.Fatalf("result = %s %+v, want one violation on relations", code, vs)
		}
	})
}

func TestIntegrationMCPMalformedTraceparentIsIgnored(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-desktop")

	created := callTool(t, sess, "artifact_create", map[string]any{
		"body":        "telemetry must not break the upload",
		"traceparent": "not-a-traceparent",
	})
	var createdOut mcpCreateOutput
	decodeToolJSON(t, created, &createdOut)

	if read := restGetArtifact(t, srv, token, createdOut.ID); read.TraceID != "" {
		t.Fatalf("trace_id = %q, want absent: a malformed traceparent is ignored, not stored", read.TraceID)
	}
}
