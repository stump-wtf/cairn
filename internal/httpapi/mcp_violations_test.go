package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-7,
// ADR-0003 (one core, thin adapters: REST and MCP report the same violations)

// toolViolations decodes a failed tool call's VE-7 structured content. A
// result with no structured content decodes to the zero code and no
// violations, which every caller treats as a failure.
func toolViolations(t *testing.T, res *mcp.CallToolResult) (errs.Code, []errs.Violation) {
	t.Helper()
	if res.StructuredContent == nil {
		return "", nil
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-encode structured content: %v", err)
	}
	var sc struct {
		Code       errs.Code        `json:"code"`
		Violations []errs.Violation `json:"violations"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return sc.Code, sc.Violations
}

// isGeneric reports whether v is the generic violation (VE-6).
func isGeneric(v errs.Violation) bool {
	g := errs.Generic()
	return v.Field == g.Field && v.Reason == g.Reason && v.Message == g.Message
}

// runToolErr packs err the way the SDK packs a typed tool handler's error and
// runs it through mcpToolErrorMiddleware, with no transport or database.
func runToolErr(t *testing.T, method string, err error) *mcp.CallToolResult {
	t.Helper()
	srv := New(nil, nil, nil, noRateLimit(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	toolErr := srv.mcpToolErr(context.Background(), "artifact_create", err)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		var res mcp.CallToolResult
		res.SetError(toolErr)
		return &res, nil
	}
	out, callErr := srv.mcpToolErrorMiddleware()(next)(context.Background(), method, nil)
	if callErr != nil {
		t.Fatalf("middleware returned a transport error: %v", callErr)
	}
	return out.(*mcp.CallToolResult)
}

// The middleware rewrites a validation failure, and only that.
func TestMCPToolErrorShapes(t *testing.T) {
	t.Run("migrated validation", func(t *testing.T) {
		res := runToolErr(t, "tools/call", errs.Violate("tags[0]", errs.LocBody, errs.ReasonUppercase, errs.WithValue("Handoff")))
		code, vs := toolViolations(t, res)
		if !res.IsError || code != errs.CodeValidation || len(vs) != 1 || vs[0].Field != "tags[0]" {
			t.Fatalf("result = IsError %v, %s %+v", res.IsError, code, vs)
		}
		if got, want := toolText(t, res), errs.Summary(vs); got != want {
			t.Fatalf("text = %q, want the REST top-level message %q", got, want)
		}
	})
	t.Run("unmigrated validation is the generic violation", func(t *testing.T) {
		res := runToolErr(t, "tools/call", errs.Validationf("create: something internal"))
		code, vs := toolViolations(t, res)
		if code != errs.CodeValidation || len(vs) != 1 || !isGeneric(vs[0]) {
			t.Fatalf("result = %s %+v, want the generic violation", code, vs)
		}
		if strings.Contains(toolText(t, res), "internal") {
			t.Fatalf("text %q leaks the core error", toolText(t, res))
		}
	})
	t.Run("non-validation text is unchanged", func(t *testing.T) {
		res := runToolErr(t, "tools/call", errs.ErrNotFound)
		if got := toolText(t, res); got != "not_found: not found or expired" {
			t.Fatalf("text = %q, want today's not_found text", got)
		}
		if res.StructuredContent != nil {
			t.Fatalf("structured content = %v, want none", res.StructuredContent)
		}
	})
	t.Run("resource reads keep the code-prefixed text", func(t *testing.T) {
		srv := New(nil, nil, nil, noRateLimit(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		err := srv.mcpToolErr(context.Background(), "resources/read", errs.Violate("id", errs.LocBody, errs.ReasonRequired))
		if got := err.Error(); got != "validation_failed: the request was invalid" {
			t.Fatalf("error text = %q, want the unchanged code-prefixed text", got)
		}
		var te *mcpToolError
		if !errors.As(err, &te) || len(te.violations) != 1 {
			t.Fatalf("error %#v does not carry its violation", err)
		}
	})
}

// VE-7 scenario "Agent learns the tag rule".
func TestIntegrationMCPViolationTagRule(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "artifact_create", map[string]any{"body": "task", "tags": []string{"Handoff"}})
	if !res.IsError {
		t.Fatal("artifact_create with tag Handoff must fail")
	}
	code, vs := toolViolations(t, res)
	if code != errs.CodeValidation || len(vs) != 1 {
		t.Fatalf("structured content = %s %+v, want one validation_failed violation", code, vs)
	}
	v := vs[0]
	if v.Field != "tags[0]" || v.Location != errs.LocBody || v.Reason != errs.ReasonUppercase || valueOf(v) != "Handoff" {
		t.Fatalf("violation = %+v, want tags[0] uppercase \"Handoff\"", v)
	}
	text := toolText(t, res)
	if text != errs.Summary(vs) || text == "validation_failed: the request was invalid" {
		t.Fatalf("text = %q, want the top-level message %q", text, errs.Summary(vs))
	}
}

// Non-validation MCP errors keep today's text and gain no structured content.
func TestIntegrationMCPNotFoundUnchanged(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, token, nil, "agent")

	res := callTool(t, sess, "artifact_read", map[string]any{"id": "nosuchid"})
	if !res.IsError || toolText(t, res) != "not_found: not found or expired" {
		t.Fatalf("artifact_read of an unknown id: IsError=%v text=%q", res.IsError, toolText(t, res))
	}
	if res.StructuredContent != nil {
		t.Fatalf("not_found structured content = %v, want none", res.StructuredContent)
	}
}

// The same bad input over REST and over MCP yields the same violations
// (SPEC-0019 VE-7, ADR-0003).
//
// Field and location legitimately differ by surface, because each names the
// input the way that surface takes it: REST reads a tag from ?tag= (field
// "tag", location query), MCP from the tags argument (field "tags[0]",
// location body). Parity is on what the violation says: reason, limit, unit,
// value and message.
//
// There is no TTL case: artifact_create takes no TTL argument (MCP always
// applies the server default), so no MCP call can send a bad one.
func TestIntegrationMCPRESTViolationParity(t *testing.T) {
	cfg := mcpConfig()
	cfg.MaxUploadBytes = 256
	opts := store.Options{MaxUploadBytes: cfg.MaxUploadBytes}
	restSrv := testServer(t, cfg, opts)
	mcpSrv, _ := mcpTestServer(t, cfg, opts)
	token := mintMCPToken(t, mcpSrv, "sam@stump.rocks", []string{"artifacts:write"})
	sess := mcpClient(t, mcpSrv, token, nil, "agent")

	cases := []struct {
		name string
		// query is appended to the REST create URL; args are the MCP call's.
		query     string
		body      string
		args      map[string]any
		code      errs.Code
		status    int
		wantField [2]string // REST, MCP
		// pending names the story that brings REST to parity, while REST
		// still reports the generic violation for this input.
		pending string
	}{
		{
			name: "uppercase tag", query: "?tag=Handoff", body: "task",
			args: map[string]any{"body": "task", "tags": []string{"Handoff"}},
			code: errs.CodeValidation, status: http.StatusBadRequest, wantField: [2]string{"tag", "tags[0]"},
		},
		{
			name: "overlong tag", query: "?tag=" + strings.Repeat("a", 65), body: "task",
			args: map[string]any{"body": "task", "tags": []string{strings.Repeat("a", 65)}},
			code: errs.CodeValidation, status: http.StatusBadRequest, wantField: [2]string{"tag", "tags[0]"},
		},
		{
			name: "oversize body", body: strings.Repeat("x", 4096),
			args: map[string]any{"body": strings.Repeat("x", 4096)},
			code: errs.CodePayloadTooLarge, status: http.StatusRequestEntityTooLarge, wantField: [2]string{"body", "body"},
		},
		{
			// MCP rejects a bundle type in its adapter with a real violation;
			// REST reaches the store's check, which #281 migrates.
			name: "bundle type", query: "?type=bundle", body: "task",
			args: map[string]any{"body": "task", "share_type": "bundle"},
			code: errs.CodeValidation, status: http.StatusBadRequest, wantField: [2]string{"type", "share_type"},
			pending: "#281",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, http.MethodPost, restSrv.URL+"/v1/artifacts"+tc.query, "sam@stump.rocks", strings.NewReader(tc.body), "text/plain")
			if resp.StatusCode != tc.status {
				resp.Body.Close()
				t.Fatalf("REST status = %d, want %d", resp.StatusCode, tc.status)
			}
			env := decodeError(t, resp)
			res := callTool(t, sess, "artifact_create", tc.args)
			mcpCode, mcpVs := toolViolations(t, res)
			if !res.IsError || mcpCode != tc.code || env.Error.Code != tc.code {
				t.Fatalf("codes: REST %s, MCP %s (IsError %v), want %s", env.Error.Code, mcpCode, res.IsError, tc.code)
			}
			restVs := env.Error.Violations
			if tc.pending != "" && len(restVs) == 1 && isGeneric(restVs[0]) {
				// Until then MCP must still say something specific.
				if len(mcpVs) != 1 || isGeneric(mcpVs[0]) || mcpVs[0].Field != tc.wantField[1] {
					t.Fatalf("MCP violations = %+v, want one on %q", mcpVs, tc.wantField[1])
				}
				t.Logf("REST reports the generic violation until %s; parity is asserted once it does not", tc.pending)
				return
			}
			if len(restVs) != 1 || len(mcpVs) != 1 {
				t.Fatalf("violations: REST %+v, MCP %+v, want one each", restVs, mcpVs)
			}
			rv, mv := restVs[0], mcpVs[0]
			if rv.Field != tc.wantField[0] || mv.Field != tc.wantField[1] {
				t.Fatalf("fields: REST %q, MCP %q, want %q and %q", rv.Field, mv.Field, tc.wantField[0], tc.wantField[1])
			}
			if rv.Reason != mv.Reason || rv.Limit != mv.Limit || rv.Unit != mv.Unit ||
				valueOf(rv) != valueOf(mv) || rv.Message != mv.Message {
				t.Fatalf("violations differ:\n REST %+v (value %q)\n MCP  %+v (value %q)", rv, valueOf(rv), mv, valueOf(mv))
			}
		})
	}
}

// a2ui_action's own argument checks carry violations too (VE-7), so an agent
// that sends a bad action learns which key to fix.
func TestIntegrationMCPA2UIActionViolations(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read"})
	sess := mcpClient(t, srv, token, nil, "a2ui-agent")

	res := callTool(t, sess, "a2ui_action", map[string]any{"name": "delete_member", "context": map[string]any{"bundle": "b", "member": "m"}})
	code, vs := toolViolations(t, res)
	if !res.IsError || code != errs.CodeValidation || len(vs) != 1 || vs[0].Field != "name" ||
		vs[0].Reason != errs.ReasonUnknownValue || valueOf(vs[0]) != "delete_member" || vs[0].Limit != `"open_member"` {
		t.Fatalf("unknown action: IsError %v, %s %+v", res.IsError, code, vs)
	}

	res = callTool(t, sess, "a2ui_action", map[string]any{"name": "open_member"})
	code, vs = toolViolations(t, res)
	if !res.IsError || code != errs.CodeValidation || len(vs) != 2 ||
		vs[0].Field != "context.bundle" || vs[1].Field != "context.member" ||
		vs[0].Reason != errs.ReasonRequired || vs[1].Reason != errs.ReasonRequired {
		t.Fatalf("missing context: IsError %v, %s %+v", res.IsError, code, vs)
	}
	if text := toolText(t, res); text != errs.Summary(vs) {
		t.Fatalf("missing context text = %q, want %q", text, errs.Summary(vs))
	}
}
