package httpapi

// Comment and Trace Redaction Tests
//
// The transport half of cairn#291: comments and traces created or appended
// over REST and MCP are masked before they are stored, their responses carry
// the outcome (SPEC-0017 RD-9) and never the token, a live append with a token
// succeeds and reports one redaction (RD-4), a span's Authorization header
// keeps its name and scheme (RD-3), and a refused field is logged with its
// surface and no value. Credentials are assembled at run time from split
// literals.
//
// Governing: ADR-0023, SPEC-0017 RD-3, RD-4, RD-7, RD-9, RD-11
//
// @joestump 09/26/2026 - Added for cairn#291.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

var (
	sharedScannerOnce sync.Once
	sharedScanner     *redact.Scanner
	sharedScannerErr  error
)

// testRedactionScanner is a real scanner with the production defaults, built
// once for the package: it is safe for concurrent use, and every harness that
// builds a Server gives it one, as cairnd does.
func testRedactionScanner(t *testing.T) *redact.Scanner {
	t.Helper()
	sharedScannerOnce.Do(func() { sharedScanner, sharedScannerErr = redact.New(redact.Config{}) })
	if sharedScannerErr != nil {
		t.Fatal(sharedScannerErr)
	}
	return sharedScanner
}

// bearerValue is an opaque bearer credential: only the Authorization label
// rule can find it.
func bearerValue() string { return "zq8" + "Lk2Vx7" + "Pn4Rt9Wm" }

// scannerServer is redactionServer with a caller-chosen scanner.
func scannerServer(t *testing.T, sc *redact.Scanner) (*httptest.Server, *syncBuffer) {
	t.Helper()
	cfg := noRateLimit()
	cfg.DevInsecureBearerAuth = true
	cfg.Redaction = sc
	logs := &syncBuffer{}
	st := store.New(newTestPool(t), objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))).Handler())
	t.Cleanup(srv.Close)
	return srv, logs
}

func postJSON(t *testing.T, url, actor string, v any) (*http.Response, string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodPost, url, actor, strings.NewReader(string(b)), "application/json")
	return resp, readBody(t, resp)
}

func wantRedactions(t *testing.T, where string, got OwnerRedaction, status string, count int, rules map[string]int) {
	t.Helper()
	if got.RedactionStatus != status || got.Redacted == nil || *got.Redacted != (count > 0) || got.Redactions == nil || got.Redactions.Count != count {
		t.Errorf("%s outcome = %+v (redactions %+v); want %s, %d", where, got, got.Redactions, status, count)
		return
	}
	for rule, n := range rules {
		if got.Redactions.Rules[rule] != n {
			t.Errorf("%s rules = %v; want %v", where, got.Redactions.Rules, rules)
		}
	}
}

// TestRedactionCommentMaskedOverREST: a comment is stored and returned masked,
// its create response carries the outcome, a listing carries none, and no
// response or log line holds the token.
func TestRedactionCommentMaskedOverREST(t *testing.T) {
	srv, _, logs := redactionServer(t)
	tok := plantedToken(11)
	id := createArtifact(t, srv.URL, "markdown", "alice", "# notes")

	resp, created := postJSON(t, srv.URL+"/v1/artifacts/"+id+"/comments", "bob",
		map[string]any{"anchor_type": "artifact", "body": "leaked " + tok + " here"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("comment = %d: %s", resp.StatusCode, created)
	}
	var c commentResponse
	if err := json.Unmarshal([]byte(created), &c); err != nil {
		t.Fatal(err)
	}
	if c.Body != "leaked [REDACTED] here" {
		t.Errorf("comment body = %q; want the token masked", c.Body)
	}
	wantRedactions(t, "comment create", c.OwnerRedaction, "masked", 1, map[string]int{"github-pat": 1})

	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/comments", "", nil, "")
	listed := readBody(t, resp)
	var list struct {
		Comments []map[string]json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil {
		t.Fatal(err)
	}
	for _, lc := range list.Comments {
		for _, k := range ownerKeys {
			if _, ok := lc[k]; ok {
				t.Errorf("a comment listing carries %q", k)
			}
		}
	}
	for where, text := range map[string]string{"create response": created, "listing": listed, "server log": logs.String()} {
		assertNoToken(t, where, text, []string{tok})
	}
}

// TestRedactionLiveAppendReportsOneRedaction: SPEC-0017 RD-4 "Live trace
// append is masked, not refused" over REST. The append succeeds, its response
// reports one redaction, the stored output is masked, and the owner's read of
// the run shows the total while an anonymous read shows none.
func TestRedactionLiveAppendReportsOneRedaction(t *testing.T) {
	srv, _, logs := redactionServer(t)
	tok := plantedToken(12)

	resp, opened := postJSON(t, srv.URL+"/v1/runs", "alice", map[string]any{"mode": "open", "title": "live"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d: %s", resp.StatusCode, opened)
	}
	var run runResponse
	if err := json.Unmarshal([]byte(opened), &run); err != nil {
		t.Fatal(err)
	}
	wantRedactions(t, "open", run.OwnerRedaction, "clean", 0, nil)

	resp, appended := postJSON(t, srv.URL+"/v1/runs/"+run.ID+"/spans", "alice", map[string]any{"spans": []spanRequest{{
		SpanID: "a1", Category: "exec", Tool: "bash", Name: "cat .env",
		Output: []byte("GITHUB_TOKEN=" + tok + "\n"), StartOffsetMS: 0, DurationMS: 5,
	}}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("append with a token = %d: %s; want 200", resp.StatusCode, appended)
	}
	var after runResponse
	if err := json.Unmarshal([]byte(appended), &after); err != nil {
		t.Fatal(err)
	}
	wantRedactions(t, "append", after.OwnerRedaction, "masked", 1, map[string]int{"github-pat": 1})
	if got := flattenSpans(after.Spans)["a1"].Output; got != "GITHUB_TOKEN=[REDACTED]\n" {
		t.Errorf("appended output = %q; want the token masked", got)
	}

	resp = do(t, http.MethodGet, srv.URL+"/v1/runs/"+run.ID, "alice", nil, "")
	ownerRaw := readBody(t, resp)
	var owner runResponse
	if err := json.Unmarshal([]byte(ownerRaw), &owner); err != nil {
		t.Fatal(err)
	}
	wantRedactions(t, "owner read", owner.OwnerRedaction, "masked", 1, map[string]int{"github-pat": 1})
	resp = do(t, http.MethodGet, srv.URL+"/v1/runs/"+run.ID, "", nil, "")
	anonRaw := readBody(t, resp)
	for _, k := range ownerKeys {
		if _, ok := jsonKeys(t, anonRaw)[k]; ok {
			t.Errorf("an anonymous run read carries %q", k)
		}
	}
	for where, text := range map[string]string{"append response": appended, "owner read": ownerRaw, "anonymous read": anonRaw, "server log": logs.String()} {
		assertNoToken(t, where, text, []string{tok})
	}
}

// TestRedactionSpanAuthorizationLabelKept: SPEC-0017 RD-3 "Label kept, value
// masked" over REST. A batch run's span args keep the Authorization header's
// name and scheme, lose only the value, and still parse.
func TestRedactionSpanAuthorizationLabelKept(t *testing.T) {
	srv, _, logs := redactionServer(t)
	bearer := bearerValue()

	resp, created := postJSON(t, srv.URL+"/v1/runs", "alice", map[string]any{"title": "api call", "spans": []spanRequest{{
		SpanID: "s1", Category: "net", Tool: "http", Name: "GET /me",
		Args:          json.RawMessage(`{"method": "GET", "headers": {"Authorization": "Bearer ` + bearer + `"}}`),
		StartOffsetMS: 0, DurationMS: 5,
	}}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %s", resp.StatusCode, created)
	}
	var run runResponse
	if err := json.Unmarshal([]byte(created), &run); err != nil {
		t.Fatal(err)
	}
	var args struct {
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(flattenSpans(run.Spans)["s1"].Args, &args); err != nil {
		t.Fatalf("stored args do not parse: %v", err)
	}
	if args.Headers["Authorization"] != "Bearer [REDACTED]" || args.Method != "GET" {
		t.Errorf("args = %+v; want the header name and scheme kept and the value masked", args)
	}
	wantRedactions(t, "create", run.OwnerRedaction, "masked", 1, map[string]int{"cairn-authorization": 1})
	if strings.Contains(created, bearer) || strings.Contains(logs.String(), bearer) {
		t.Error("the bearer value reached a response or the log")
	}
}

// TestRedactionRefusedRunFieldLogged: SPEC-0017 RD-7 and RD-9 "Logs carry no
// value" on the run surface. A prompt over the scan cap is refused as
// validation_failed and nothing is stored; the log line names the field, the
// surface and the reason, and holds none of the field's text.
func TestRedactionRefusedRunFieldLogged(t *testing.T) {
	small, err := redact.New(redact.Config{MaxScanBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	srv, logs := scannerServer(t, small)
	tok := plantedToken(13)
	prompt := strings.Repeat("padding ", 40) + tok

	resp, body := postJSON(t, srv.URL+"/v1/runs", "alice", map[string]any{"title": "big", "prompt": prompt})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize prompt = %d: %s; want 400", resp.StatusCode, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if env.Error.Code != "validation_failed" {
		t.Errorf("code = %q; want validation_failed", env.Error.Code)
	}
	log := logs.String()
	for _, want := range []string{"surface=run", "redaction_reason=too_large_to_scan", "prompt: "} {
		if !strings.Contains(log, want) {
			t.Errorf("log line lacks %q:\n%s", want, log)
		}
	}
	for where, text := range map[string]string{"response": body, "server log": log} {
		assertNoToken(t, where, text, []string{tok})
		if strings.Contains(text, "padding padding") {
			t.Errorf("%s echoes the refused field", where)
		}
	}
}

// TestRedactionMCPRunAndCommentCarryOutcome: run_append_spans and
// artifact_comment store masked text and report their own outcome, and
// neither tool result holds the token.
func TestRedactionMCPRunAndCommentCarryOutcome(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	sess := mcpClient(t, srv, mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write", "annotations:write"}), nil, "claude-code")
	tok := plantedToken(14)

	var opened mcpRunOutput
	decodeToolJSON(t, callTool(t, sess, "run_create", map[string]any{"mode": "open", "title": "live"}), &opened)
	wantRedactions(t, "run_create", opened.OwnerRedaction, "clean", 0, nil)

	res := callTool(t, sess, "run_append_spans", map[string]any{"id": opened.ID, "spans": []map[string]any{{
		"span_id": "a1", "category": "exec", "tool": "bash", "name": "export GH_TOKEN=" + tok,
		"start_offset_ms": 0, "duration_ms": 5,
	}}})
	var appended mcpRunOutput
	decodeToolJSON(t, res, &appended)
	wantRedactions(t, "run_append_spans", appended.OwnerRedaction, "masked", 1, nil)
	assertNoToken(t, "run_append_spans result", toolText(t, res), []string{tok})

	var art mcpCreateOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_create", map[string]any{"body": "hello"}), &art)
	res = callTool(t, sess, "artifact_comment", map[string]any{"id": art.ID, "anchor_type": "artifact", "body": "key: " + tok})
	var comment mcpCommentOutput
	decodeToolJSON(t, res, &comment)
	if !strings.Contains(comment.Body, redact.Mask) {
		t.Errorf("comment body = %q; want the token masked", comment.Body)
	}
	wantRedactions(t, "artifact_comment", comment.OwnerRedaction, "masked", 1, nil)
	assertNoToken(t, "artifact_comment result", toolText(t, res), []string{tok})
}
