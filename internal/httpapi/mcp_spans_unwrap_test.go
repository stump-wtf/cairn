package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/store"
)

func TestUnwrapStringEncodedSpans(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantSame bool // true if output should equal input (no fixup)
	}{
		{
			name:     "native array unchanged",
			input:    `{"id":"test","spans":[{"span_id":"s1","category":"exec"}]}`,
			wantSame: true,
		},
		{
			name:     "string-encoded spans unwrapped",
			input:    `{"id":"test","spans":"[{\"span_id\":\"s1\",\"category\":\"exec\"}]"}`,
			wantSame: false,
		},
		{
			name:     "null spans unchanged",
			input:    `{"id":"test","spans":null}`,
			wantSame: true,
		},
		{
			name:     "missing spans unchanged",
			input:    `{"id":"test"}`,
			wantSame: true,
		},
		{
			name:     "empty object unchanged",
			input:    `{}`,
			wantSame: true,
		},
		{
			name:     "invalid JSON returned as-is",
			input:    `not json`,
			wantSame: true,
		},
		{
			name:     "string that is not valid array left as-is",
			input:    `{"id":"test","spans":"not an array"}`,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapStringEncodedSpans(json.RawMessage(tt.input))
			if tt.wantSame {
				if string(got) != tt.input {
					t.Errorf("unwrapStringEncodedSpans() = %s, want unchanged %s", string(got), tt.input)
				}
			} else {
				// Verify the result is valid JSON with spans as an array
				var args map[string]json.RawMessage
				if err := json.Unmarshal(got, &args); err != nil {
					t.Fatalf("result is not valid JSON: %v", err)
				}
				spansRaw, ok := args["spans"]
				if !ok {
					t.Fatal("result missing spans field")
				}
				// Verify spans is now an array (starts with '[')
				if len(spansRaw) == 0 || spansRaw[0] != '[' {
					t.Errorf("spans should be an array, got: %s", string(spansRaw))
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
