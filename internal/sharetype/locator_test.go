package sharetype

import (
	"encoding/json"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// typeForAnchor maps each anchor to a built-in share type that declares it, so
// ValidateLocator resolves a type whose LocatorSchemer carries the schema.
var typeForAnchor = map[Anchor]artifact.ShareType{
	AnchorArtifact:           artifact.TypeFile,
	AnchorMarkdownBlock:      KeyMarkdown,
	AnchorMarkdownBullet:     KeyMarkdown,
	AnchorTextSelection:      KeyMarkdown,
	AnchorCodeLine:           KeyCode,
	AnchorCodeRange:          KeyCode,
	AnchorImageRegion:        KeyImage,
	AnchorBundleFile:         artifact.TypeBundle,
	AnchorWebhookRequest:     KeyWebhook,
	AnchorTrajectorySpan:     KeyTrajectory,
	AnchorTrajectoryTurn:     KeyTrajectory,
	AnchorTrajectoryToolCall: KeyTrajectory,
}

// TestLocatorSchemas feeds valid and malformed anchor_ref payloads per
// anchor_type and asserts schema validation (ADR-0006 "A locator schema test
// feeds valid and malformed anchor_ref payloads per anchor_type").
func TestLocatorSchemas(t *testing.T) {
	cases := []struct {
		anchor  Anchor
		valid   []string
		invalid []string
	}{
		{
			anchor:  AnchorArtifact,
			valid:   []string{``, `{}`, ` { } `, `null`},
			invalid: []string{`{"x":1}`, `[]`, `"artifact"`, `42`},
		},
		{
			anchor: AnchorMarkdownBlock,
			valid:  []string{`{"block_id":"b_3f2a"}`},
			invalid: []string{
				``, `{}`, `{"block_id":""}`, `{"block_id":42}`,
				`{"block_id":"b_1","extra":true}`, `[]`, `null`, `{"block_id":"b_1"} {}`,
			},
		},
		{
			anchor: AnchorMarkdownBullet,
			valid:  []string{`{"block_id":"b_9c1","path":[2,0]}`, `{"path":[0],"block_id":"b_9c1"}`},
			invalid: []string{
				`{"block_id":"b_9c1"}`, `{"path":[2,0]}`, `{"block_id":"b_9c1","path":[]}`,
				`{"block_id":"b_9c1","path":[-1]}`, `{"block_id":"","path":[0]}`,
				`{"block_id":"b_9c1","path":["a"]}`,
			},
		},
		{
			anchor: AnchorTextSelection,
			valid:  []string{`{"start":1201,"end":1240,"quote":"then we deploy"}`, `{"start":0,"end":1,"quote":"x"}`},
			invalid: []string{
				`{}`, `{"start":1201,"end":1240}`, `{"start":10,"end":5,"quote":"q"}`,
				`{"start":10,"end":10,"quote":"q"}`, `{"start":-1,"end":4,"quote":"q"}`,
				`{"start":1,"end":2,"quote":""}`, `{"start":"a","end":2,"quote":"q"}`,
			},
		},
		{
			anchor: AnchorCodeLine,
			valid:  []string{`{"line":42}`, `{"line":1,"hash":"9c81d04e"}`},
			invalid: []string{
				`{}`, `{"line":0}`, `{"line":-3}`, `{"line":"42"}`,
				`{"line":42,"hash":""}`, `{"line":42,"column":1}`,
			},
		},
		{
			anchor: AnchorCodeRange,
			valid:  []string{`{"start":40,"end":47}`, `{"start":7,"end":7}`},
			invalid: []string{
				`{}`, `{"start":40}`, `{"end":47}`, `{"start":0,"end":4}`,
				`{"start":9,"end":8}`, `{"start":40,"end":47,"file":"a.go"}`,
			},
		},
		{
			anchor: AnchorImageRegion,
			valid:  []string{`{"x":0.42,"y":0.31}`, `{"x":0,"y":1}`, `{"x":0.1,"y":0.2,"w":0.3,"h":0.4}`},
			invalid: []string{
				`{}`, `{"x":0.42}`, `{"y":0.31}`, `{"x":1.2,"y":0.3}`, `{"x":-0.1,"y":0.3}`,
				`{"x":0.1,"y":0.2,"w":0.3}`, `{"x":0.1,"y":0.2,"h":0.4}`,
				`{"x":0.1,"y":0.2,"w":1.5,"h":0.4}`, `{"x":420,"y":310}`,
			},
		},
		{
			anchor: AnchorBundleFile,
			valid:  []string{`{"name":"notes.md"}`, `{"block_id":"b_3f2a","name":"notes.md"}`},
			invalid: []string{
				`{}`, `{"name":""}`, `{"name":7}`, `{"file":"notes.md"}`,
				`{"name":"notes.md","block_id":""}`, `{"name":"notes.md","extra":true}`,
			},
		},
		{
			anchor:  AnchorWebhookRequest,
			valid:   []string{`{"request_id":"req_7Kx9"}`},
			invalid: []string{`{}`, `{"request_id":""}`, `{"request_id":7}`, `{"id":"req_7Kx9"}`},
		},
		{
			anchor:  AnchorTrajectorySpan,
			valid:   []string{`{"span_id":"sp_c3aa"}`},
			invalid: []string{`{}`, `{"span_id":""}`, `{"span_id":1}`, `{"turn":"sp_c3aa"}`},
		},
		{
			anchor:  AnchorTrajectoryTurn,
			valid:   []string{`{"span_id":"sp_a1bb"}`},
			invalid: []string{`{}`, `{"span_id":""}`},
		},
		{
			anchor:  AnchorTrajectoryToolCall,
			valid:   []string{`{"span_id":"sp_b2cc"}`},
			invalid: []string{`{}`, `{"span_id":""}`},
		},
	}

	reg := Default()
	for _, tc := range cases {
		key := typeForAnchor[tc.anchor]
		for _, ref := range tc.valid {
			if err := reg.ValidateLocator(key, tc.anchor, json.RawMessage(ref)); err != nil {
				t.Errorf("%s: valid ref %q rejected: %v", tc.anchor, ref, err)
			}
		}
		for _, ref := range tc.invalid {
			err := reg.ValidateLocator(key, tc.anchor, json.RawMessage(ref))
			if err == nil {
				t.Errorf("%s: malformed ref %q accepted", tc.anchor, ref)
				continue
			}
			if errs.CodeOf(err) != errs.CodeValidation {
				t.Errorf("%s: malformed ref %q code = %q, want validation_failed", tc.anchor, ref, errs.CodeOf(err))
			}
		}
	}
}

// TestBuiltinTypesSupplyLocatorSchemas asserts every built-in type implements
// the LocatorSchemer capability and supplies a schema for each non-artifact
// anchor it declares, so no declared anchor accepts arbitrary payloads.
func TestBuiltinTypesSupplyLocatorSchemas(t *testing.T) {
	for _, st := range Default().Types() {
		ls, ok := st.(LocatorSchemer)
		if !ok {
			t.Errorf("type %q does not implement LocatorSchemer", st.Key())
			continue
		}
		for _, spec := range st.Anchors() {
			if spec.Anchor == AnchorArtifact {
				continue // enforced centrally by ValidateLocator
			}
			if ls.LocatorSchema(spec.Anchor) == nil {
				t.Errorf("type %q declares anchor %q with no locator schema", st.Key(), spec.Anchor)
			}
		}
	}
}
