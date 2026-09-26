package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Governing: ADR-0023, SPEC-0017 RD-5, RD-9; SPEC-0007 REQ "Agent-Shaped Tool
// Schemas"

// publishedTools lists the tools the real MCP server publishes, over an
// in-memory transport, exactly as a client sees them on tools/list. No store
// is needed: listing never calls a handler.
func publishedTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.newMCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "schema-test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

// schemaObject is a JSON schema as a client decodes it.
type schemaObject struct {
	Type        any                      `json:"type"`
	Description string                   `json:"description"`
	Enum        []any                    `json:"enum"`
	Required    []string                 `json:"required"`
	Properties  map[string]*schemaObject `json:"properties"`
}

func decodeSchema(t *testing.T, raw any) *schemaObject {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var s schemaObject
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return &s
}

// hasType reports whether s admits JSON type want, either as its one type or
// as a member of a union (an output schema keeps the SDK's ["null", X]).
func hasType(s *schemaObject, want string) bool {
	switch t := s.Type.(type) {
	case string:
		return t == want
	case []any:
		return slices.Contains(t, any(want))
	}
	return false
}

// TestMCPCreateToolsDocumentRedaction: artifact_create and bundle_create
// publish an optional string redaction argument whose description names
// "mask" as its only value and says scanning cannot be turned off (SPEC-0017
// RD-5); their descriptions say what is stored is scanned; and their output
// schemas carry redacted and redactions {count, rules} (RD-9).
func TestMCPCreateToolsDocumentRedaction(t *testing.T) {
	tools := publishedTools(t)
	for _, name := range []string{"artifact_create", "bundle_create"} {
		t.Run(name, func(t *testing.T) {
			tool, ok := tools[name]
			if !ok {
				t.Fatalf("%s is not published", name)
			}
			if !strings.Contains(tool.Description, "scanned for credentials") || !strings.Contains(tool.Description, "redaction argument") {
				t.Errorf("description does not mention the credential scan and the redaction argument: %q", tool.Description)
			}

			in := decodeSchema(t, tool.InputSchema)
			arg, ok := in.Properties["redaction"]
			if !ok {
				t.Fatalf("input schema has no redaction property")
			}
			if !hasType(arg, "string") {
				t.Errorf("redaction type = %q, want string", arg.Type)
			}
			if slices.Contains(in.Required, "redaction") {
				t.Error("redaction is required; the default mode must apply when it is omitted")
			}
			// An enum would have the SDK refuse "off" with its own schema
			// error, where RD-5 requires validation_failed / unknown_value.
			if len(arg.Enum) != 0 {
				t.Errorf("redaction enum = %v, want none: the handler must be the one to refuse a value", arg.Enum)
			}
			for _, want := range []string{`the only value is "mask"`, "[REDACTED]", "validation_failed", "scanning cannot be turned off", "redactions {count, rules}"} {
				if !strings.Contains(arg.Description, want) {
					t.Errorf("redaction description lacks %q: %q", want, arg.Description)
				}
			}

			out := decodeSchema(t, tool.OutputSchema)
			if p, ok := out.Properties["redacted"]; !ok || !hasType(p, "boolean") {
				t.Errorf("output schema redacted = %+v, want a boolean property", p)
			}
			red, ok := out.Properties["redactions"]
			if !ok {
				t.Fatal("output schema has no redactions property")
			}
			for _, field := range []string{"count", "rules"} {
				if _, ok := red.Properties[field]; !ok {
					t.Errorf("output schema redactions has no %q", field)
				}
			}
		})
	}
}
