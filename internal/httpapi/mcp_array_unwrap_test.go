package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/store"
)

func TestUnwrapStringEncodedArrays(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		field    string
		wantSame bool // true if output should equal input (no fixup)
	}{
		{
			name:     "native array unchanged",
			input:    `{"id":"test","spans":[{"span_id":"s1","category":"exec"}]}`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "string-encoded spans unwrapped",
			input:    `{"id":"test","spans":"[{\"span_id\":\"s1\",\"category\":\"exec\"}]"}`,
			field:    "spans",
			wantSame: false,
		},
		{
			name:     "null spans unchanged",
			input:    `{"id":"test","spans":null}`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "missing spans unchanged",
			input:    `{"id":"test"}`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "empty object unchanged",
			input:    `{}`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "invalid JSON returned as-is",
			input:    `not json`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "string that is not valid array left as-is",
			input:    `{"id":"test","spans":"not an array"}`,
			field:    "spans",
			wantSame: true,
		},
		{
			name:     "whitespace-prefixed string still unwrapped",
			input:    `{"id":"test","spans":  "[{\"span_id\":\"s1\",\"category\":\"exec\"}]"}`,
			field:    "spans",
			wantSame: false,
		},
		{
			name:     "string-encoded bundle members unwrapped",
			input:    `{"title":"t","members":"[{\"name\":\"a.md\",\"body\":\"hi\"}]"}`,
			field:    "members",
			wantSame: false,
		},
		{
			name:     "native bundle members unchanged",
			input:    `{"title":"t","members":[{"name":"a.md","body":"hi"}]}`,
			field:    "members",
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapStringEncodedArrays(json.RawMessage(tt.input), tt.field)
			if tt.wantSame {
				if string(got) != tt.input {
					t.Errorf("unwrapStringEncodedArrays() = %s, want unchanged %s", string(got), tt.input)
				}
			} else {
				// Verify the result is valid JSON with the field as an array
				var args map[string]json.RawMessage
				if err := json.Unmarshal(got, &args); err != nil {
					t.Fatalf("result is not valid JSON: %v", err)
				}
				fieldRaw, ok := args[tt.field]
				if !ok {
					t.Fatalf("result missing %s field", tt.field)
				}
				// Verify the field is now an array (starts with '[')
				if len(fieldRaw) == 0 || fieldRaw[0] != '[' {
					t.Errorf("%s should be an array, got: %s", tt.field, string(fieldRaw))
				}
			}
		})
	}
}

// TestIntegrationMCPStringEncodedSpansUnwrapped is the end-to-end regression
// for issue #108 (dup #111): an MCP client that double-encodes the spans
// array as a JSON string — the exact shape Claude Code sends after its
// schema view drops the ["null","array"] union type — must still land its
// spans. The unit test above proves the unwrap transform; this proves the
// middleware actually runs BEFORE the SDK's schema validation on the real
// streamable-HTTP transport, for both run_create (batch) and
// run_append_spans, and that a string that is not a JSON array still fails
// validation instead of being silently dropped.
func TestIntegrationMCPStringEncodedSpansUnwrapped(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	// run_create (batch) with the spans array serialized as a string.
	created := callTool(t, sess, "run_create", map[string]any{
		"title": "string-encoded spans",
		"spans": `[{"span_id":"root-1","category":"exec","tool":"bash","name":"run the suite","start_offset_ms":0,"duration_ms":500}]`,
	})
	var runOut mcpRunOutput
	decodeToolJSON(t, created, &runOut)
	if runOut.ID == "" || runOut.Status != "closed" {
		t.Fatalf("run_create with string-encoded spans = %+v, want a closed run", runOut)
	}
	if runOut.Stats.SpanCount != 1 {
		t.Fatalf("run stats = %+v, want span_count 1", runOut.Stats)
	}

	// run_append_spans against a live run, spans again string-encoded.
	opened := callTool(t, sess, "run_create", map[string]any{
		"mode":  "open",
		"title": "string-encoded append",
	})
	var liveOut mcpRunOutput
	decodeToolJSON(t, opened, &liveOut)
	if liveOut.ID == "" || liveOut.Status != "open" {
		t.Fatalf("run_create mode=open = %+v, want an open run", liveOut)
	}
	appended := callTool(t, sess, "run_append_spans", map[string]any{
		"id":    liveOut.ID,
		"spans": `[{"span_id":"s1","category":"read","tool":"read_file","name":"inspect","start_offset_ms":0,"duration_ms":40},{"span_id":"s2","parent_span_id":"s1","category":"reason","name":"think","start_offset_ms":10,"duration_ms":20}]`,
	})
	var afterAppend mcpRunOutput
	decodeToolJSON(t, appended, &afterAppend)
	if afterAppend.Stats.SpanCount != 2 {
		t.Fatalf("append stats = %+v, want span_count 2", afterAppend.Stats)
	}

	// A string that does not decode to a JSON array must NOT be unwrapped
	// into anything — schema validation still rejects it loudly.
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "run_append_spans",
		Arguments: map[string]any{"id": liveOut.ID, "spans": "not an array"},
	})
	if err == nil && !res.IsError {
		t.Fatalf("run_append_spans with a non-array string succeeded (%s), want a validation error", toolText(t, res))
	}
	// And the failed call must not have grown the run.
	after := callTool(t, sess, "run_append_spans", map[string]any{
		"id":    liveOut.ID,
		"spans": `[{"span_id":"s1","category":"read","tool":"read_file","name":"inspect","start_offset_ms":0,"duration_ms":40}]`,
	})
	var idempotent mcpRunOutput
	decodeToolJSON(t, after, &idempotent)
	if idempotent.Stats.SpanCount != 2 {
		t.Fatalf("span_count after rejected garbage = %d, want the original 2", idempotent.Stats.SpanCount)
	}
}

// TestIntegrationMCPStringEncodedMembersUnwrapped is the bundle_create half of
// the same regression: a client that string-encodes the members array — the
// shape it falls back to when its schema view has dropped the parameter's
// type — must still land its bundle. bundle_create was unreachable over MCP
// until the published schema stopped carrying a ["null","array"] union; this
// covers the clients still holding that flattened schema.
func TestIntegrationMCPStringEncodedMembersUnwrapped(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	created := callTool(t, sess, "bundle_create", map[string]any{
		"title":   "string-encoded members",
		"members": `[{"name":"README.md","body":"# hi","media_type":"text/markdown"},{"name":"notes.txt","body":"second"}]`,
	})
	var out mcpBundleCreateOutput
	decodeToolJSON(t, created, &out)
	if out.ID == "" {
		t.Fatalf("bundle_create with string-encoded members = %+v, want a created bundle", out)
	}
	if len(out.Members) != 2 {
		t.Fatalf("members = %+v, want 2", out.Members)
	}
	if out.Members[0].Name != "README.md" || out.Members[1].Name != "notes.txt" {
		t.Fatalf("members = %+v, want README.md then notes.txt in bundle order", out.Members)
	}

	// A string that does not decode to a JSON array must still be rejected
	// rather than silently swallowed into an empty bundle.
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "bundle_create",
		Arguments: map[string]any{"title": "junk", "members": "not an array"},
	})
	if err == nil && !res.IsError {
		t.Fatalf("bundle_create with a non-array string succeeded (%s), want a validation error", toolText(t, res))
	}
}
