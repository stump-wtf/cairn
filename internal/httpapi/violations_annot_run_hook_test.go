package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/trajectory"
	"github.com/stump-wtf/cairn/internal/webhook"
)

// Governing: ADR-0025, SPEC-0019 VE-1, VE-3, VE-4, VE-6 (#283: annotations,
// runs and hooks)

func init() { guardSiteSources = append(guardSiteSources, annotationRunHookSites) }

// annotationRunHookSites are the validators outside the create path (#283).
// Every service below is built with no pool: each site rejects before any
// database or object store is touched, so the guard runs everywhere.
func annotationRunHookSites(t *testing.T) []guardSite {
	ctx := context.Background()
	reg := sharetype.Default()
	annot := annotation.NewService(nil, reg)
	traj := trajectory.NewService(nil, nil, trajectory.Options{MaxOutputBytes: 4})
	hooks := webhook.NewService(nil, nil, webhook.Options{})
	s := New(nil, nil, nil, Config{MaxRunRequestBytes: 8}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	validate := func(st artifact.ShareType, kind sharetype.AnnotationKind, a sharetype.Anchor, ref string) func() error {
		return func() error { _, err := annotation.Validate(reg, 1, st, kind, a, json.RawMessage(ref)); return err }
	}
	root := annotation.ReplyParent{Exists: true, AnchorType: sharetype.AnchorCodeLine, AnchorRef: json.RawMessage(`{"line":1}`), AnchorKey: `{"line":1}`}
	reply := func(p annotation.ReplyParent, a sharetype.Anchor, ref string) func() error {
		return func() error { _, _, err := annotation.CheckReply(7, p, a, json.RawMessage(ref)); return err }
	}
	decode := func(dec func(http.ResponseWriter, *http.Request, any) error, body string) func() error {
		return func() error {
			var dst map[string]any
			return dec(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), &dst)
		}
	}

	now := time.Now()
	runIn := func(spans ...trajectory.SpanInput) trajectory.RunInput {
		return trajectory.RunInput{
			StartedAt:  now,
			Provenance: artifact.Provenance{ActorID: "alice", Channel: artifact.ChannelAPI, CapturedAt: now},
			Access:     artifact.AccessPolicy{OwnerID: "alice", Visibility: artifact.VisibilityLink},
			ExpiresAt:  now.Add(time.Hour),
			Spans:      spans,
		}
	}
	batch := func(spans ...trajectory.SpanInput) func() error {
		return func() error { _, err := traj.CreateBatchRun(ctx, runIn(spans...)); return err }
	}
	span := func(id, parent string, cat trajectory.Category) trajectory.SpanInput {
		return trajectory.SpanInput{SpanID: id, ParentSpanID: parent, Category: cat}
	}

	hookReq := func(query, seq string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/hooks/h/requests?"+query, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("seq", seq)
		return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	}
	hookIn := webhook.EndpointInput{
		RequestCap: -1,
		Provenance: artifact.Provenance{ActorID: "alice", Channel: artifact.ChannelAPI, CapturedAt: now},
		Access:     artifact.AccessPolicy{OwnerID: "alice", Visibility: artifact.VisibilityLink},
		ExpiresAt:  now.Add(time.Hour),
	}

	return []guardSite{
		// Annotations.
		{"annotation: emoji empty", func() error {
			_, _, err := annot.React(ctx, "x", sharetype.AnchorArtifact, nil, "", "alice")
			return err
		}},
		{"annotation: emoji malformed", func() error {
			_, _, err := annot.React(ctx, "x", sharetype.AnchorArtifact, nil, "🔥 🔥", "alice")
			return err
		}},
		{"annotation: unreact emoji", func() error {
			_, err := annot.Unreact(ctx, "x", sharetype.AnchorArtifact, nil, "", "alice")
			return err
		}},
		{"annotation: unreact anchor_ref", func() error {
			_, err := annot.Unreact(ctx, "x", sharetype.AnchorCodeLine, json.RawMessage(`[1]`), "🔥", "alice")
			return err
		}},
		{"annotation: comment body empty", func() error {
			_, err := annot.AddComment(ctx, "x", annotation.CommentInput{ActorID: "alice"})
			return err
		}},
		{"annotation: comment body too long", func() error {
			_, err := annot.AddComment(ctx, "x", annotation.CommentInput{ActorID: "alice", Body: strings.Repeat("a", 16<<10+1)})
			return err
		}},
		{"annotation: edit body empty", func() error { return annot.EditComment(ctx, "x", 1, "alice", "") }},
		{"annotation: anchor_type not allowed", validate(sharetype.KeyCode, sharetype.KindReaction, sharetype.AnchorImageRegion, `{"x":0.5,"y":0.5}`)},
		{"annotation: anchor_type required", validate(sharetype.KeyCode, sharetype.KindComment, "", ``)},
		{"annotation: not commentable", validate(sharetype.KeyWebhook, sharetype.KindComment, sharetype.AnchorArtifact, ``)},
		{"annotation: anchor_ref locator", validate(sharetype.KeyCode, sharetype.KindComment, sharetype.AnchorCodeLine, `{"line":0}`)},
		{"annotation: anchor_ref not canonical", validate(artifact.TypeFile, sharetype.KindComment, sharetype.AnchorArtifact, `{}{}`)},
		{"annotation: reply parent not found", reply(annotation.ReplyParent{}, "", ``)},
		{"annotation: reply thread too deep", reply(annotation.ReplyParent{Exists: true, IsReply: true}, "", ``)},
		{"annotation: reply anchor_type mismatch", reply(root, sharetype.AnchorArtifact, ``)},
		{"annotation: reply anchor_ref mismatch", reply(root, sharetype.AnchorCodeLine, `{"line":2}`)},
		{"annotation: reply anchor_ref malformed", reply(root, sharetype.AnchorCodeLine, `nope`)},
		{"annotation: body not JSON", decode(s.decodeAnnotationBody, "{")},
		{"annotation: body over the cap", decode(s.decodeAnnotationBody, `{"body":"`+strings.Repeat("a", maxAnnotationRequestBytes)+`"}`)},
		{"annotation: reaction id", func() error { _, err := parseReactionID("abc"); return err }},

		// Runs.
		{"run: mode", func() error { return checkRunMode("stream") }},
		{"run+hook: body not JSON", decode(s.decodeJSONBody, "{")},
		{"run+hook: body over the cap", decode(s.decodeJSONBody, `{"title":"too long"}`)},
		{"run: span category empty", batch(span("s1", "", ""))},
		{"run: span category too long", batch(span("s1", "", trajectory.Category(strings.Repeat("c", trajectory.MaxCategoryLen+1))))},
		{"run: span unknown parent", batch(span("s1", "ghost", trajectory.CategoryReason))},
		{"run: span own parent", batch(span("s1", "s1", trajectory.CategoryReason))},
		{"run: span parent cycle", batch(span("a", "b", trajectory.CategoryReason), span("b", "a", trajectory.CategoryReason))},
		{"run: span_id missing", batch(span("", "", trajectory.CategoryReason))},
		{"run: span_id duplicate", batch(span("s1", "", trajectory.CategoryReason), span("s1", "", trajectory.CategoryReason))},
		{"run: tool on a reason span", batch(trajectory.SpanInput{SpanID: "s1", Category: trajectory.CategoryReason, Tool: "bash"})},
		{"run: produced artifact on a non-write span", batch(trajectory.SpanInput{SpanID: "s1", Category: trajectory.CategoryRead, ProducedArtifactID: "abc"})},
		{"run: negative timing", batch(trajectory.SpanInput{SpanID: "s1", Category: trajectory.CategoryExec, StartOffsetMS: -1, DurationMS: -1})},
		{"run: open with a bad seed span", func() error {
			_, err := traj.OpenRun(ctx, runIn(span("s1", "ghost", trajectory.CategoryReason)))
			return err
		}},
		{"run: span output over the cap", batch(trajectory.SpanInput{SpanID: "s1", Category: trajectory.CategoryExec, Output: []byte("12345")})},
		{"run: append with no spans", func() error { _, err := traj.AppendSpans(ctx, "x", "alice", nil); return err }},

		// Hooks.
		{"hook: request_cap negative", func() error { _, err := hooks.CreateEndpoint(ctx, hookIn); return err }},
		{"hook: list before", func() error { _, _, err := parseHookListQuery(hookReq("before=x", "1")); return err }},
		{"hook: list limit", func() error { _, _, err := parseHookListQuery(hookReq("limit=-1", "1")); return err }},
		{"hook: seq", func() error { _, err := parseSeq(hookReq("", "0")); return err }},

		// MCP adapters. A decoded JSON object cannot fail to re-encode, so a
		// NaN (which json.Marshal rejects) stands in for the unreachable case.
		{"mcp: span args", func() error {
			_, err := toRunSpanInputs([]mcpRunSpanInput{{SpanID: "s1", Args: map[string]any{"x": math.NaN()}}})
			return err
		}},
		{"mcp: anchor_ref", func() error {
			_, err := mcpAnchorInput{AnchorRef: map[string]any{"x": math.NaN()}}.anchorRefJSON()
			return err
		}},
	}
}

// The MCP span-args check names every bad span by index in one error, and the
// anchor_ref check names anchor_ref; both stay validation_failed.
func TestViolationMCPSpanArgsAndAnchorRef(t *testing.T) {
	nan := map[string]any{"x": math.NaN()}
	_, err := toRunSpanInputs([]mcpRunSpanInput{
		{SpanID: "a", Args: nan},
		{SpanID: "b", Args: map[string]any{"ok": true}},
		{SpanID: "c", Args: nan},
	})
	if errs.CodeOf(err) != errs.CodeValidation {
		t.Fatalf("span args: code = %q, want validation_failed (err %v)", errs.CodeOf(err), err)
	}
	if got := fields(errs.ViolationsOf(err)); strings.Join(got, ",") != "spans[0].args invalid_format,spans[2].args invalid_format" {
		t.Fatalf("span args violations = %v, want spans 0 and 2", got)
	}

	spans, err := toRunSpanInputs([]mcpRunSpanInput{{SpanID: "a", Args: map[string]any{"ok": true}}})
	if err != nil || len(spans) != 1 || string(spans[0].Args) != `{"ok":true}` {
		t.Fatalf("valid args: spans = %+v, err = %v", spans, err)
	}

	_, err = mcpAnchorInput{AnchorRef: nan}.anchorRefJSON()
	vs := errs.ViolationsOf(err)
	if errs.CodeOf(err) != errs.CodeValidation || len(vs) != 1 || vs[0].Field != "anchor_ref" ||
		vs[0].Location != errs.LocBody || vs[0].Reason != errs.ReasonInvalidFormat || vs[0].Value != nil {
		t.Fatalf("anchor_ref: err = %v, violations = %+v", err, vs)
	}
}

// The sentinels callers already test with errors.Is survive migration, and
// each maps to validation_failed.
func TestAnnotationRunHookSentinelsSurvive(t *testing.T) {
	root := annotation.ReplyParent{Exists: true, AnchorType: sharetype.AnchorCodeLine, AnchorRef: json.RawMessage(`{"line":1}`), AnchorKey: `{"line":1}`}
	_, _, notFound := annotation.CheckReply(1, annotation.ReplyParent{}, "", nil)
	_, _, tooDeep := annotation.CheckReply(1, annotation.ReplyParent{Exists: true, IsReply: true}, "", nil)
	_, _, mismatch := annotation.CheckReply(1, root, sharetype.AnchorArtifact, nil)
	_, notCommentable := annotation.Validate(sharetype.Default(), 1, sharetype.KeyWebhook, sharetype.KindComment, sharetype.AnchorArtifact, nil)
	for sentinel, err := range map[error]error{
		annotation.ErrParentNotFound: notFound,
		annotation.ErrThreadTooDeep:  tooDeep,
		annotation.ErrAnchorMismatch: mismatch,
		annotation.ErrNotCommentable: notCommentable,
	} {
		if !errors.Is(err, sentinel) || !errors.Is(err, errs.ErrValidation) || errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("%v: err = %v, want the sentinel and validation_failed", sentinel, err)
		}
	}
}

// A malformed or oversize annotation body is named on the wire: body
// invalid_format (400), or body too_large at the 32 KiB cap (413).
func TestViolationAnnotationBody(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	post := func(body string) *http.Response {
		return do(t, http.MethodPost, srv.URL+"/v1/artifacts/abc/comments", "alice", strings.NewReader(body), "application/json")
	}

	resp := post("{")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	v := decodeError(t, resp).Error.Violations
	if len(v) != 1 || v[0].Field != "body" || v[0].Reason != errs.ReasonInvalidFormat || !strings.Contains(v[0].Message, "a JSON object") {
		t.Fatalf("violations = %+v", v)
	}

	resp = post(`{"body":"` + strings.Repeat("a", maxAnnotationRequestBytes) + `"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if v := env.Error.Violations; env.Error.Code != errs.CodePayloadTooLarge || len(v) != 1 || v[0].Field != "body" ||
		v[0].Reason != errs.ReasonTooLarge || v[0].Limit != float64(maxAnnotationRequestBytes) || v[0].Value != nil {
		t.Fatalf("error = %+v, want body too_large at the cap, no echo", env.Error)
	}
}

// The reaction id in the path is named, with its value echoed.
func TestViolationReactionID(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	resp := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/abc/reactions/xyz", "alice", nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "rid" || v[0].Location != errs.LocPath || valueOf(v[0]) != "xyz" {
		t.Fatalf("violations = %+v", v)
	}
	if env.Error.Details["id"] != "abc" {
		t.Fatalf("details = %v, want the handler's id kept", env.Error.Details)
	}
}

// postJSON sends v as JSON and asserts the status, returning the envelope.
func postJSONError(t *testing.T, url string, v any, want int) errorEnvelope {
	t.Helper()
	var body io.Reader
	if s, ok := v.(string); ok {
		body = strings.NewReader(s)
	} else {
		body = jsonReader(t, v)
	}
	resp := do(t, http.MethodPost, url, "alice", body, "application/json")
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status = %d, want %d: %s", url, resp.StatusCode, want, b)
	}
	return decodeError(t, resp)
}

func fields(vs []errs.Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Field + " " + string(v.Reason)
	}
	return out
}

// Annotation violations render over REST: emoji, anchor, parent and body. The
// comment body is never echoed.
func TestIntegrationViolationAnnotations(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	code := createArtifact(t, srv.URL, "code", "alice", "package main\n")
	base := srv.URL + "/v1/artifacts/" + code

	env := postJSONError(t, base+"/reactions", reactionRequest{AnchorType: "artifact", Emoji: "not an emoji"}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "emoji" || v[0].Reason != errs.ReasonInvalidFormat || valueOf(v[0]) != "not an emoji" {
		t.Fatalf("emoji: %+v", v)
	}

	env = postJSONError(t, base+"/reactions", reactionRequest{AnchorType: "image_region", AnchorRef: json.RawMessage(`{"x":0.5,"y":0.5}`), Emoji: "🔥"}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "anchor_type" || v[0].Reason != errs.ReasonNotAllowed ||
		valueOf(v[0]) != "image_region" || !strings.Contains(v[0].Message, "code_line") {
		t.Fatalf("anchor_type: %+v", v)
	}
	if env.Error.Details["anchor_type"] != "image_region" || env.Error.Details["id"] != code {
		t.Fatalf("details = %v, want the handler's id and anchor_type kept", env.Error.Details)
	}

	env = postJSONError(t, base+"/comments", commentRequest{AnchorType: "code_line", AnchorRef: json.RawMessage(`{"line":0}`), Body: "x"}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "anchor_ref" || v[0].Reason != errs.ReasonInvalidFormat {
		t.Fatalf("anchor_ref: %+v", v)
	}

	missing := int64(424242)
	env = postJSONError(t, base+"/comments", commentRequest{ParentID: &missing, Body: "orphan"}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "parent_id" || v[0].Reason != errs.ReasonNotAllowed || valueOf(v[0]) != strconv.FormatInt(missing, 10) {
		t.Fatalf("parent_id: %+v", v)
	}

	secret := strings.Repeat("s", 16<<10+1)
	env = postJSONError(t, base+"/comments", commentRequest{AnchorType: "artifact", Body: secret}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "body" || v[0].Reason != errs.ReasonTooLong || v[0].Value != nil || v[0].Limit != float64(16<<10) {
		t.Fatalf("body: %+v", v)
	}

	hook := createArtifact(t, srv.URL, "webhook", "alice", "x")
	env = postJSONError(t, srv.URL+"/v1/artifacts/"+hook+"/comments", commentRequest{AnchorType: "artifact", Body: "nope"}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "anchor_type" || !strings.Contains(v[0].Message, "accepts no comments") {
		t.Fatalf("not commentable: %+v", v)
	}
}

// VE-4 over REST: a run batch reports every structural problem at once, and
// a mixed append names each already-present span.
func TestIntegrationViolationRuns(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	env := postJSONError(t, srv.URL+"/v1/runs", runRequest{StartedAt: fixedRunStart, Spans: []spanRequest{
		{SpanID: "s1", Category: "reason"},
		{SpanID: "s2", Category: ""},
		{SpanID: "s3", ParentSpanID: "ghost", Category: "exec"},
	}}, http.StatusBadRequest)
	if got := fields(env.Error.Violations); strings.Join(got, ",") != "spans[1].category required,spans[2].parent_span_id not_allowed" {
		t.Fatalf("violations = %v", got)
	}
	if !strings.HasPrefix(env.Error.Message, "2 problems: spans[1].category: ") {
		t.Fatalf("message = %q", env.Error.Message)
	}

	env = postJSONError(t, srv.URL+"/v1/runs", runRequest{Mode: "stream", StartedAt: fixedRunStart}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "mode" || v[0].Reason != errs.ReasonUnknownValue || valueOf(v[0]) != "stream" {
		t.Fatalf("mode: %+v", v)
	}

	env = postJSONError(t, srv.URL+"/v1/runs", runRequest{StartedAt: fixedRunStart, Spans: []spanRequest{
		{SpanID: "w", Category: "write", ProducedArtifactID: "NOPE0000"},
	}}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "spans[0].produced_artifact_id" || valueOf(v[0]) != "NOPE0000" {
		t.Fatalf("produced: %+v", v)
	}

	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "alice",
		jsonReader(t, runRequest{Mode: "open", StartedAt: fixedRunStart, Spans: []spanRequest{{SpanID: "s1", Category: "reason"}}}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open run: status %d", resp.StatusCode)
	}
	run := decodeRun(t, resp)
	env = postJSONError(t, srv.URL+"/v1/runs/"+run.ID+"/spans", appendSpansRequest{Spans: []spanRequest{
		{SpanID: "s2", Category: "exec"},
		{SpanID: "s1", Category: "reason"},
	}}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "spans[1].span_id" || v[0].Reason != errs.ReasonDuplicate || valueOf(v[0]) != "s1" {
		t.Fatalf("mixed append: %+v", v)
	}

	env = postJSONError(t, srv.URL+"/v1/runs/"+run.ID+"/spans", appendSpansRequest{}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "spans" || v[0].Reason != errs.ReasonRequired {
		t.Fatalf("empty append: %+v", v)
	}

	env = postJSONError(t, srv.URL+"/v1/runs", "{", http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "body" || v[0].Reason != errs.ReasonInvalidFormat {
		t.Fatalf("bad JSON: %+v", v)
	}
}

// The span output cap is named per span and renders as payload_too_large. The
// cap is enforced before any storage is touched, so no pool is needed.
func TestViolationSpanOutputTooLarge(t *testing.T) {
	svc := trajectory.NewService(nil, nil, trajectory.Options{MaxOutputBytes: 4})
	now := time.Now()
	_, err := svc.CreateBatchRun(context.Background(), trajectory.RunInput{
		StartedAt:  now,
		Provenance: artifact.Provenance{ActorID: "alice", Channel: artifact.ChannelAPI, CapturedAt: now},
		Access:     artifact.AccessPolicy{OwnerID: "alice", Visibility: artifact.VisibilityLink},
		ExpiresAt:  now.Add(time.Hour),
		Spans: []trajectory.SpanInput{
			{SpanID: "ok", Category: trajectory.CategoryExec, Output: []byte("tiny")},
			{SpanID: "big", Category: trajectory.CategoryExec, Output: []byte("too big")},
		},
	})
	rec := renderError(t, err, nil, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "spans[1].output" || v[0].Reason != errs.ReasonTooLarge || v[0].Limit != float64(4) || v[0].Value != nil {
		t.Fatalf("violations = %+v", v)
	}
}

// Hook create and list validation renders over REST.
func TestIntegrationViolationHooks(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	env := postJSONError(t, srv.URL+"/v1/hooks", createHookRequest{RequestCap: -3}, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "request_cap" || v[0].Reason != errs.ReasonNotPositive || valueOf(v[0]) != "-3" {
		t.Fatalf("request_cap: %+v", v)
	}
	env = postJSONError(t, srv.URL+"/v1/hooks", `{"request_cap":"many"}`, http.StatusBadRequest)
	if v := env.Error.Violations; len(v) != 1 || v[0].Field != "body" || v[0].Reason != errs.ReasonInvalidFormat {
		t.Fatalf("bad JSON: %+v", v)
	}

	resp := do(t, http.MethodPost, srv.URL+"/v1/hooks", "alice", jsonReader(t, createHookRequest{}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create hook: status %d", resp.StatusCode)
	}
	var hook hookResponse
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, srv.URL+"/v1/hooks/"+hook.ID+"/requests?before=x&limit=-1", "", nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("list: status %d, want 400", resp.StatusCode)
	}
	if got := fields(decodeError(t, resp).Error.Violations); strings.Join(got, ",") != "before invalid_format,limit invalid_format" {
		t.Fatalf("list violations = %v, want both query params", got)
	}

	resp = do(t, http.MethodGet, srv.URL+"/v1/hooks/"+hook.ID+"/requests/0", "", nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("seq: status %d, want 400", resp.StatusCode)
	}
	if v := decodeError(t, resp).Error.Violations; len(v) != 1 || v[0].Field != "seq" || v[0].Location != errs.LocPath {
		t.Fatalf("seq: %+v", v)
	}
}
