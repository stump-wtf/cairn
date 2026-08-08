package httpapi

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
