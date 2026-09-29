package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/errs"
)

// TestCollapseNullableUnions covers the transform itself: a `["null", X]`
// union becomes plain X wherever it appears, and a union that is genuinely
// multi-typed is left multi-typed.
func TestCollapseNullableUnions(t *testing.T) {
	t.Run("nil is safe", func(t *testing.T) {
		if got := collapseNullableUnions(nil); got != nil {
			t.Fatalf("collapseNullableUnions(nil) = %v, want nil", got)
		}
	})

	t.Run("nullable array collapses to array", func(t *testing.T) {
		s := &jsonschema.Schema{Types: []string{"null", "array"}}
		collapseNullableUnions(s)
		if s.Type != "array" || len(s.Types) != 0 {
			t.Fatalf("Type=%q Types=%v, want Type=array and no Types", s.Type, s.Types)
		}
	})

	t.Run("genuinely multi-typed union keeps its members", func(t *testing.T) {
		s := &jsonschema.Schema{Types: []string{"string", "number"}}
		collapseNullableUnions(s)
		if s.Type != "" || len(s.Types) != 2 {
			t.Fatalf("Type=%q Types=%v, want the two-member union preserved", s.Type, s.Types)
		}
	})

	t.Run("a bare null type is left alone", func(t *testing.T) {
		s := &jsonschema.Schema{Types: []string{"null"}}
		collapseNullableUnions(s)
		if len(s.Types) != 1 || s.Types[0] != "null" {
			t.Fatalf("Types=%v, want the null type untouched rather than a typeless schema", s.Types)
		}
	})

	t.Run("nested unions are reached", func(t *testing.T) {
		s := &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"members": {
					Types: []string{"null", "array"},
					Items: &jsonschema.Schema{
						Type: "object",
						Properties: map[string]*jsonschema.Schema{
							"tags": {Types: []string{"null", "array"}},
						},
					},
				},
			},
		}
		collapseNullableUnions(s)
		members := s.Properties["members"]
		if members.Type != "array" {
			t.Fatalf("members.Type = %q, want array", members.Type)
		}
		if got := members.Items.Properties["tags"].Type; got != "array" {
			t.Fatalf("members.items.tags.Type = %q, want array — nested unions must be collapsed too", got)
		}
	})
}

// TestToolInputSchemasAreClientRepresentable is the regression for the defect
// that made bundle_create uncallable over MCP: the SDK infers a nil-able Go
// slice/pointer as a `["null", X]` union, and clients that cannot represent a
// union type drop the whole subschema to `{}`. The parameter then looks
// untyped, the client sends whatever it likes, and the server's own validator
// — which still holds the union — rejects it. So: no published tool input
// schema may contain a union type anywhere, and the parameters that bit us
// must publish as concrete arrays with usable item schemas.
func TestToolInputSchemasAreClientRepresentable(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		schema func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
		// arrayFields must each publish as a plain array with item schemas.
		arrayFields []string
	}{
		{"bundle_create", jsonschema.For[mcpBundleCreateInput], []string{"members"}},
		{"run_create", jsonschema.For[mcpRunCreateInput], []string{"spans"}},
		{"run_append_spans", jsonschema.For[mcpRunAppendInput], []string{"spans"}},
		{"artifact_create", jsonschema.For[mcpCreateInput], nil},
		{"artifact_comment", jsonschema.For[mcpCommentInput], nil},
		{"artifact_react", jsonschema.For[mcpReactInput], nil},
		{"artifact_read", jsonschema.For[mcpReadInput], nil},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			inferred, err := tc.schema(nil)
			if err != nil {
				t.Fatalf("infer: %v", err)
			}
			schema := collapseNullableUnions(inferred)

			if where := findUnionType(schema, "$"); where != "" {
				t.Errorf("%s publishes a union type at %s; clients that cannot represent one drop the subschema to {} and the parameter becomes unusable", tc.tool, where)
			}

			for _, field := range tc.arrayFields {
				prop, ok := schema.Properties[field]
				if !ok {
					t.Fatalf("%s has no %q property", tc.tool, field)
				}
				if prop.Type != "array" {
					t.Errorf("%s.%s Type = %q, want a plain \"array\"", tc.tool, field, prop.Type)
				}
				if prop.Items == nil || len(prop.Items.Properties) == 0 {
					t.Errorf("%s.%s has no item schema; an agent cannot tell what a member looks like", tc.tool, field)
				}
			}

			// The schema must still survive a JSON round trip as an object —
			// this is what actually goes out on tools/list.
			raw, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out["type"] != "object" {
				t.Errorf("%s input schema type = %v, want object as the spec requires", tc.tool, out["type"])
			}
		})
	}
}

// TestAddToolPublishesCollapsedSchema proves the wiring, not just the
// transform: registering a tool whose In type has a nil-able slice must
// publish a plain array. Without addTool the SDK would infer `["null",
// "array"]` here, so this fails the moment a registration goes back to
// mcp.AddTool directly.
func TestAddToolPublishesCollapsedSchema(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tool := &mcp.Tool{Name: "probe", Description: "probe"}
	addTool(srv, tool, func(context.Context, *mcp.CallToolRequest, mcpBundleCreateInput) (*mcp.CallToolResult, mcpBundleCreateOutput, error) {
		return nil, mcpBundleCreateOutput{}, nil
	})

	schema, ok := tool.InputSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("InputSchema is %T, want *jsonschema.Schema", tool.InputSchema)
	}
	if where := findUnionType(schema, "$"); where != "" {
		t.Fatalf("addTool published a union type at %s", where)
	}
	if got := schema.Properties["members"].Type; got != "array" {
		t.Fatalf("members.Type = %q, want array", got)
	}
}

// findUnionType returns a JSON-pointer-ish path to the first schema carrying a
// multi-member type union, or "" if there is none.
func findUnionType(s *jsonschema.Schema, path string) string {
	if s == nil {
		return ""
	}
	if len(s.Types) > 1 {
		return path
	}
	for name, sub := range s.Properties {
		if where := findUnionType(sub, path+".properties."+name); where != "" {
			return where
		}
	}
	for name, sub := range s.Defs {
		if where := findUnionType(sub, path+".$defs."+name); where != "" {
			return where
		}
	}
	if where := findUnionType(s.Items, path+".items"); where != "" {
		return where
	}
	if where := findUnionType(s.AdditionalProperties, path+".additionalProperties"); where != "" {
		return where
	}
	for i, sub := range s.AnyOf {
		if where := findUnionType(sub, path+".anyOf["+strconv.Itoa(i)+"]"); where != "" {
			return where
		}
	}
	return ""
}

// successOutputPayload is a minimal complete artifact_create success value as
// JSON: every key the success branch requires, correctly typed. It pins that
// widening the schema for tool errors did not loosen the success shape.
func successOutputPayload() map[string]any {
	return map[string]any{
		"id": "a1b2c3d4e5f6", "url": "https://cairn.stump.wtf/a/a1b2c3d4e5f6",
		"mcp": "mcp://cairn/a1b2c3d4e5f6", "share_type": "markdown",
		"size": 42, "media_type": "text/markdown", "previewable": true,
		"badge": "new", "visibility": "link",
		"provenance": map[string]any{
			"actor": "u1", "channel": "via MCP", "captured_at": "2026-09-29T11:00:00Z",
		},
		"reaction_count": 0, "comment_count": 0, "pin_count": 0,
		"created_at": "2026-09-29T11:00:00Z", "expires_at": "2026-10-06T11:00:00Z",
		"expires_in": "in 6d", "expires_in_seconds": 604800,
	}
}

// toolErrorPayload is the VE-7 structured content mcpToolErrorMiddleware
// attaches to a failed call, built like the real one: the tag-rule violation
// the acceptance test drives.
func toolErrorPayload() mcpToolErrorContent {
	return mcpToolErrorContent{
		Code: errs.CodeValidation,
		Violations: []errs.Violation{
			errs.NewViolation("tags[0]", errs.LocBody, errs.ReasonUppercase,
				errs.WithLimit(5, "chars"), errs.WithValue("Handoff")),
		},
	}
}

// jsonRoundTrip passes v through one encode/decode cycle, which is both what
// the wire does to it and what jsonschema-go's Validate needs (it cannot
// walk a Go struct directly).
func jsonRoundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// TestAddToolWidensOutputSchemaForToolErrors is the regression for the TS-SDK
// break: a typed tool's published outputSchema must admit both the success
// shape and the VE-7 error content, because the TypeScript SDK validates
// every result carrying structuredContent — error results included — against
// the schema from tools/list, and throws McpError(InvalidParams) on a
// mismatch, losing the violations it was sent to deliver. The widening must
// stay exact: a payload matching neither branch still fails.
func TestAddToolWidensOutputSchemaForToolErrors(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tool := &mcp.Tool{Name: "probe", Description: "probe"}
	addTool(srv, tool, func(context.Context, *mcp.CallToolRequest, mcpCreateInput) (*mcp.CallToolResult, mcpCreateOutput, error) {
		return nil, mcpCreateOutput{}, nil
	})

	schema, ok := tool.OutputSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("OutputSchema is %T, want *jsonschema.Schema", tool.OutputSchema)
	}
	if schema.Type != "object" {
		t.Fatalf("root Type = %q, want object — the outputSchema shape clients require", schema.Type)
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("root has %d anyOf branches, want success + tool error", len(schema.AnyOf))
	}

	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{})
	if err != nil {
		t.Fatalf("resolve widened schema: %v", err)
	}
	if err := resolved.Validate(successOutputPayload()); err != nil {
		t.Errorf("success payload rejected by the widened schema: %v", err)
	}
	if err := resolved.Validate(jsonRoundTrip(t, toolErrorPayload())); err != nil {
		t.Errorf("VE-7 error content rejected by the tool's own outputSchema — a TS-SDK client would throw instead of showing the violation: %v", err)
	}
	if err := resolved.Validate(map[string]any{"nope": 1}); err == nil {
		t.Error("payload matching neither branch validated; the widening must stay exact")
	}
}

// TestAddToolKeepsUntypedOutputSchemaNil pins the a2ui tools' shape: an Out of
// `any` publishes no outputSchema (the SDK infers none), so the middleware's
// error content on those tools has no schema to fail and no client validator
// to trip.
func TestAddToolKeepsUntypedOutputSchemaNil(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tool := &mcp.Tool{Name: "probe", Description: "probe"}
	addTool(srv, tool, func(context.Context, *mcp.CallToolRequest, mcpA2UIActionInput) (*mcp.CallToolResult, any, error) {
		return nil, nil, nil
	})
	if tool.OutputSchema != nil {
		t.Fatalf("Out=any tool published an outputSchema of %T, want none", tool.OutputSchema)
	}
}

// TestWidenedOutputSchemasHaveNoBooleanSchemas pins every typed tool's
// widened output schema to schema objects, never a bare `true` — what
// jsonschema-go emits for an `any`-typed field (a violation's limit, an
// anchor_ref, a run span). Strict clients have dropped whole tools over
// exactly that (all ten vanished from Claude Code once, over five input
// fields); the published output side must not reintroduce it.
func TestWidenedOutputSchemasHaveNoBooleanSchemas(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		schema func(*jsonschema.ForOptions) (*jsonschema.Schema, error)
	}{
		{"artifact_read", jsonschema.For[mcpReadOutput]},
		{"artifact_create", jsonschema.For[mcpCreateOutput]},
		{"bundle_create", jsonschema.For[mcpBundleCreateOutput]},
		{"artifact_comment", jsonschema.For[mcpCommentOutput]},
		{"artifact_react", jsonschema.For[mcpReactOutput]},
		{"run_create", jsonschema.For[mcpRunOutput]},
		{"run_append_spans", jsonschema.For[mcpRunOutput]},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			inferred, err := tc.schema(nil)
			if err != nil {
				t.Fatalf("infer: %v", err)
			}
			widened := widenForToolErrors(tc.tool, collapseNullableUnions(inferred))
			if where := findBooleanSchema(widened, "$"); where != "" {
				t.Errorf("%s output schema carries a bare `true` schema at %s; strict clients drop tools over those", tc.tool, where)
			}
		})
	}
}

// findBooleanSchema returns a JSON-pointer-ish path to the first schema
// marshalling as `true` (the empty, always-valid schema), or "" if there is
// none.
func findBooleanSchema(s *jsonschema.Schema, path string) string {
	if s == nil {
		return ""
	}
	if raw, err := s.MarshalJSON(); err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		return path
	}
	for name, sub := range s.Properties {
		if where := findBooleanSchema(sub, path+".properties."+name); where != "" {
			return where
		}
	}
	for name, sub := range s.Defs {
		if where := findBooleanSchema(sub, path+".$defs."+name); where != "" {
			return where
		}
	}
	if where := findBooleanSchema(s.Items, path+".items"); where != "" {
		return where
	}
	if where := findBooleanSchema(s.AdditionalProperties, path+".additionalProperties"); where != "" {
		return where
	}
	for i, sub := range s.AnyOf {
		if where := findBooleanSchema(sub, path+".anyOf["+strconv.Itoa(i)+"]"); where != "" {
			return where
		}
	}
	return ""
}
