// Tool input and output schema shaping for the MCP surface.
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
// The output side has the mirror-image problem. A tool's outputSchema is
// inferred from its Out struct, so it describes only success — but a failed
// call also carries structuredContent (VE-7), and the TypeScript SDK
// validates that against the published schema, errors included. widenForToolErrors
// (below) admits the error shape alongside the success shape instead.
//
// Governing: SPEC-0007 REQ "Create & Push", REQ "Agent-Shaped Tool Schemas";
// SPEC-0019 VE-7.
package httpapi

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addTool registers a tool with a client-representable input schema and an
// output schema that admits both the success shape and the structured error
// shape a failed call carries. It is a drop-in for mcp.AddTool and every
// tool on this surface goes through it, so no future tool can reintroduce a
// union-typed parameter or republish a success-only output schema by
// accident. Schemas set explicitly on the tool are kept untouched.
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
	if t.OutputSchema == nil && reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		schema, err := jsonschema.For[Out](nil)
		if err != nil {
			panic(fmt.Sprintf("addTool: tool %q: infer output schema: %v", t.Name, err))
		}
		t.OutputSchema = widenForToolErrors(t.Name, collapseNullableUnions(schema))
	}
	mcp.AddTool(srv, t, h)
}

// widenForToolErrors wraps a tool's success output schema so the published
// outputSchema also admits the VE-7 structured error content
// (mcpToolErrorContent) that mcpToolErrorMiddleware attaches to a failed
// call.
//
// Without this the error content fails the tool's own published schema — it
// is not the success object, and the success schema is closed
// (additionalProperties: false) — and the TypeScript SDK client, which
// validates every result carrying structuredContent, error results included,
// throws McpError(InvalidParams) instead of showing the agent the violations.
// The agent then loses both the violations and the message, which is worse
// than no structured errors at all.
//
// The root keeps "type": "object", the shape the MCP spec and every client
// require of an outputSchema, and anyOfs the two branches. The widening is
// exact, not a loosening: `required` is what tells the branches apart, so a
// payload that is neither a success nor a {code, violations} error still
// fails. The error branch is inferred from mcpToolErrorContent itself, so it
// cannot drift from what the middleware actually marshals.
func widenForToolErrors(tool string, success *jsonschema.Schema) *jsonschema.Schema {
	toolError, err := jsonschema.For[mcpToolErrorContent](nil)
	if err != nil {
		panic(fmt.Sprintf("addTool: tool %q: infer tool-error output schema: %v", tool, err))
	}
	collapseNullableUnions(toolError)

	root := &jsonschema.Schema{
		Type:  "object",
		AnyOf: []*jsonschema.Schema{success, toolError},
	}
	if len(success.Defs) > 0 || len(toolError.Defs) > 0 {
		root.Defs = mergeSchemaDefs(tool, success.Defs, toolError.Defs)
	}
	stripBooleanSchemas(root)
	return root
}

// mergeSchemaDefs merges the two branches' $defs into the widened root, where
// any "#/$defs/..." reference inside a branch resolves. A key defined by both
// is a construction-time ambiguity that would silently repoint one branch's
// references, so it panics like any other inference failure in addTool.
func mergeSchemaDefs(tool string, a, b map[string]*jsonschema.Schema) map[string]*jsonschema.Schema {
	merged := make(map[string]*jsonschema.Schema, len(a)+len(b))
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		if _, dup := merged[k]; dup {
			panic(fmt.Sprintf("addTool: tool %q: $defs key %q is defined by both output branches", tool, k))
		}
		merged[k] = v
	}
	return merged
}

// stripBooleanSchemas replaces every schema that is just `true` — what
// jsonschema-go emits for an `any`-typed field, e.g. a violation's `limit` —
// with a description-only schema object. A boolean schema admits exactly the
// same instances, but strict clients have dropped whole tools over a bare
// `true` where a schema object was expected (all ten of this surface's tools
// vanished from Claude Code once, over five such fields in input schemas),
// and an object that carries a description says the same thing while being
// something every client can represent. A real constraint, notably
// `additionalProperties: false`, marshals as `false`, not `true`, and is
// left alone.
func stripBooleanSchemas(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if raw, err := s.MarshalJSON(); err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		*s = jsonschema.Schema{Description: "the violated limit, as any JSON value"}
		return
	}
	for _, sub := range s.Properties {
		stripBooleanSchemas(sub)
	}
	for _, sub := range s.PatternProperties {
		stripBooleanSchemas(sub)
	}
	for _, sub := range s.Defs {
		stripBooleanSchemas(sub)
	}
	for _, sub := range s.Definitions {
		stripBooleanSchemas(sub)
	}
	for _, group := range [][]*jsonschema.Schema{s.AllOf, s.AnyOf, s.OneOf, s.PrefixItems, s.ItemsArray} {
		for _, sub := range group {
			stripBooleanSchemas(sub)
		}
	}
	for _, sub := range []*jsonschema.Schema{
		s.Items, s.AdditionalItems, s.AdditionalProperties, s.UnevaluatedItems,
		s.UnevaluatedProperties, s.Contains, s.Not, s.If, s.Then, s.Else,
	} {
		stripBooleanSchemas(sub)
	}
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
