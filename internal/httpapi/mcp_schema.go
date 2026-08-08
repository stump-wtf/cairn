// Tool input schema shaping for the MCP surface.
//
// The SDK infers a tool's input schema from its handler's In type. For Go's
// nil-able kinds — a slice, a map, a pointer — that inference is faithful to
// Go and hostile to clients: it emits a *union* type, `"type": ["null",
// "array"]`, because a nil slice marshals to JSON null. JSON Schema permits
// that; a great many MCP clients do not. Faced with a type they cannot
// represent as a single string, they drop the whole subschema to `{}` — which
// leaves the parameter looking untyped, so the client sends the value however
// it likes (commonly a JSON-encoded string), and the server's own validator —
// which still holds the real union schema — rejects it. The agent then sees a
// server demanding an array its published schema gave no way to send.
//
// That is not hypothetical: it is exactly how `bundle_create` became
// uncallable over MCP, and it is the same defect the string-encoded-array
// unwrap in mcp.go was added to paper over for `run_create`'s spans.
//
// So the schema published for a tool collapses those unions to the single
// concrete type. Nothing is loosened by this: a required field was never
// legitimately null, and an optional one is still omissible — `required` is
// what governs presence, not a null branch in the type. What changes is that
// the parameter arrives at the client as `{"type": "array", "items": {...}}`,
// which every client can represent, so it sends an array and validation
// passes on the first try.
//
// Governing: SPEC-0007 REQ "Create & Push", REQ "Agent-Shaped Tool Schemas".
package httpapi

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addTool registers a tool with a client-representable input schema. It is a
// drop-in for mcp.AddTool and every tool on this surface goes through it, so
// no future tool can reintroduce a union-typed parameter by accident. A tool
// that sets InputSchema explicitly keeps it untouched.
//
// Inference failure panics, matching mcp.AddTool's own contract: a tool whose
// schema cannot be derived is a programming error caught at construction, not
// a degraded surface discovered by an agent at call time.
func addTool[In, Out any](srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if t.InputSchema == nil {
		schema, err := jsonschema.For[In](nil)
		if err != nil {
			panic(fmt.Sprintf("addTool: tool %q: infer input schema: %v", t.Name, err))
		}
		t.InputSchema = collapseNullableUnions(schema)
	}
	mcp.AddTool(srv, t, h)
}

// collapseNullableUnions rewrites every `["null", X]` union in s to plain `X`,
// in place, walking the whole schema so a union nested inside an array's items
// or a $defs entry is caught too. A union with more than one non-null member
// keeps its remaining members: that is a genuine multi-type field, not the
// nil-able-Go-kind artefact this exists to undo.
func collapseNullableUnions(s *jsonschema.Schema) *jsonschema.Schema {
	if s == nil {
		return nil
	}
	if len(s.Types) > 0 {
		concrete := make([]string, 0, len(s.Types))
		for _, t := range s.Types {
			if t != "null" {
				concrete = append(concrete, t)
			}
		}
		switch len(concrete) {
		case 0:
			// A literal `"type": "null"` — the only honest rendering is to
			// leave it alone rather than emit a typeless schema.
		case 1:
			// Type and Types are mutually exclusive in this package; setting
			// one demands clearing the other.
			s.Type, s.Types = concrete[0], nil
		default:
			s.Types = concrete
		}
	}

	for _, sub := range s.Properties {
		collapseNullableUnions(sub)
	}
	for _, sub := range s.PatternProperties {
		collapseNullableUnions(sub)
	}
	for _, sub := range s.Defs {
		collapseNullableUnions(sub)
	}
	for _, sub := range s.Definitions {
		collapseNullableUnions(sub)
	}
	for _, group := range [][]*jsonschema.Schema{s.AllOf, s.AnyOf, s.OneOf, s.PrefixItems, s.ItemsArray} {
		for _, sub := range group {
			collapseNullableUnions(sub)
		}
	}
	for _, sub := range []*jsonschema.Schema{
		s.Items, s.AdditionalItems, s.AdditionalProperties, s.UnevaluatedItems,
		s.UnevaluatedProperties, s.Contains, s.Not, s.If, s.Then, s.Else,
	} {
		collapseNullableUnions(sub)
	}
	return s
}
