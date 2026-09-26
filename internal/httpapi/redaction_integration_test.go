package httpapi

// Owner-Visible Redaction Outcome Tests
//
// SPEC-0017 RD-9 at the transport: create responses (REST, MCP) carry
// redacted and redactions, the owner sees the recorded outcome on every read
// surface (REST, MCP artifact_read, the web viewer), a non-owner sees none of
// it, and no response, log line or rendered page holds a planted token.
//
// The create paths scan (cairn#292), so the "Owner sees a summary" outcome is
// the one the create itself recorded over a body holding real tokens.
// Credentials are assembled at run time from split literals.
//
// Governing: ADR-0023, SPEC-0017 RD-9, "Accessibility Requirements"
//
// @joestump 09/25/2026 - Added for cairn#290.
// @joestump 09/26/2026 - cairn#292: creates are scanned, so these tests post
// raw tokens instead of seeding the outcome, and seed "unscanned" only to
// stand in for a legacy row.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

// plantedToken returns a GitHub personal access token shape (ghp_ plus 36
// alphanumerics) that gitleaks' github-pat rule detects, assembled at run time.
func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

// syncBuffer is a log sink safe for the server's concurrent handlers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// redactionServer is testServer with the store and the log output in reach.
func redactionServer(t *testing.T) (*httptest.Server, *store.Store, *syncBuffer) {
	t.Helper()
	cfg := noRateLimit()
	cfg.DevInsecureBearerAuth = true
	logs := &syncBuffer{}
	st := store.New(newTestPool(t), objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))).Handler())
	t.Cleanup(srv.Close)
	return srv, st, logs
}

// tokenBody is a markdown body holding two planted tokens, and the tokens.
func tokenBody() (string, []string) {
	a, b := plantedToken(1), plantedToken(2)
	return "# deploy\n\nfirst " + a + "\nsecond " + b + "\n", []string{a, b}
}

// maskedOutcome is what the scanner records for tokenBody.
var maskedOutcome = map[string]int{"github-pat": 2}

// seedOutcome records s on the artifact. The zero Summary stands in for a row
// written before the scan existed ("unscanned").
func seedOutcome(t *testing.T, st *store.Store, publicID string, s redact.Summary) {
	t.Helper()
	ctx := context.Background()
	a, err := st.GetByPublicID(ctx, publicID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.RecordRedaction(ctx, tx, store.RedactionArtifact, a.ID, s); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// assertNoToken fails if any planted token, or a long run of one, is in out.
func assertNoToken(t *testing.T, where, out string, tokens []string) {
	t.Helper()
	for _, tok := range tokens {
		if strings.Contains(out, tok) || strings.Contains(out, tok[4:24]) {
			t.Errorf("%s holds part of a planted token", where)
		}
	}
}

// ownerKeys are the JSON keys only an owner may see.
var ownerKeys = []string{"redaction_status", "redacted", "redactions"}

func jsonKeys(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return m
}

func wantOwnerOutcome(t *testing.T, got artifactResponse, status string, count int, rules map[string]int) {
	t.Helper()
	if got.RedactionStatus != status {
		t.Errorf("redaction_status = %q, want %q", got.RedactionStatus, status)
	}
	if got.Redacted == nil || *got.Redacted != (status == "masked" && count > 0) {
		t.Errorf("redacted = %v, want %v", got.Redacted, status == "masked" && count > 0)
	}
	if got.Redactions == nil || got.Redactions.Count != count || !reflect.DeepEqual(got.Redactions.Rules, rules) {
		t.Errorf("redactions = %+v, want count %d rules %v", got.Redactions, count, rules)
	}
}

// TestRedactionOwnerSeesSummary: SPEC-0017 RD-9 "Owner sees a summary" over
// REST. The owner reads masked, a count of 2 and the rule twice; the token
// appears nowhere in any response or log line. A non-owner and an anonymous
// reader see no outcome fields at all.
func TestRedactionOwnerSeesSummary(t *testing.T) {
	srv, _, logs := redactionServer(t)
	body, tokens := tokenBody()

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=deploy", "alice", strings.NewReader(body), "text/markdown")
	created := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %s", resp.StatusCode, created)
	}
	var art artifactResponse
	if err := json.Unmarshal([]byte(created), &art); err != nil {
		t.Fatal(err)
	}
	wantOwnerOutcome(t, art, "masked", 2, maskedOutcome)

	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "alice", nil, "")
	ownerRaw := readBody(t, resp)
	var owner artifactResponse
	if err := json.Unmarshal([]byte(ownerRaw), &owner); err != nil {
		t.Fatal(err)
	}
	wantOwnerOutcome(t, owner, "masked", 2, map[string]int{"github-pat": 2})

	for _, who := range []string{"mallory", ""} {
		resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, who, nil, "")
		raw := readBody(t, resp)
		for _, k := range ownerKeys {
			if _, ok := jsonKeys(t, raw)[k]; ok {
				t.Errorf("viewer %q sees %q", who, k)
			}
		}
		assertNoToken(t, "non-owner read", raw, tokens)
	}

	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID+"/body", "alice", nil, "")
	assertNoToken(t, "stored body", readBody(t, resp), tokens)
	assertNoToken(t, "create response", created, tokens)
	assertNoToken(t, "owner read", ownerRaw, tokens)
	assertNoToken(t, "server log", logs.String(), tokens)
}

// TestRedactionLegacyArtifactReadsUnscanned: an artifact with no recorded
// outcome reads "unscanned" to its owner (SPEC-0017 "Legacy artifact status").
func TestRedactionLegacyArtifactReadsUnscanned(t *testing.T) {
	srv, st, _ := redactionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "alice", "# old")
	seedOutcome(t, st, id, redact.Summary{})
	resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "alice", nil, "")
	wantOwnerOutcome(t, decodeArtifact(t, resp), "unscanned", 0, map[string]int{})
}

// TestRedactionCreateResponsesCarryOutcome: every REST create shape (raw,
// single multipart file, multipart bundle) carries redacted and redactions,
// and a body with nothing in it is scanned clean.
func TestRedactionCreateResponsesCarryOutcome(t *testing.T) {
	srv, _, _ := redactionServer(t)
	for _, files := range [][]string{{"one.txt"}, {"a.txt", "b.txt"}} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, name := range files {
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte("content of " + name))
		}
		_ = mw.Close()
		resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", &buf, mw.FormDataContentType())
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %v = %d", files, resp.StatusCode)
		}
		wantOwnerOutcome(t, decodeArtifact(t, resp), "clean", 0, map[string]int{})
	}
}

// TestRedactionMCPOwnerOnly: artifact_create and bundle_create carry the
// outcome; artifact_read shows it to the owner and not to another user.
func TestRedactionMCPOwnerOnly(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	body, tokens := tokenBody()
	owner := mcpClient(t, srv, mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"}), nil, "claude-desktop")

	res := callTool(t, owner, "artifact_create", map[string]any{"body": body, "title": "deploy"})
	var created mcpCreateOutput
	decodeToolJSON(t, res, &created)
	wantOwnerOutcome(t, created.artifactResponse, "masked", 2, maskedOutcome)
	assertNoToken(t, "artifact_create output", toolText(t, res), tokens)

	res = callTool(t, owner, "bundle_create", map[string]any{"members": []map[string]any{{"name": "a.md", "body": "a"}, {"name": "b.md", "body": "b"}}})
	var bundle mcpBundleCreateOutput
	decodeToolJSON(t, res, &bundle)
	wantOwnerOutcome(t, bundle.artifactResponse, "clean", 0, map[string]int{})

	res = callTool(t, owner, "artifact_read", map[string]any{"id": created.ID})
	var read mcpReadOutput
	decodeToolJSON(t, res, &read)
	wantOwnerOutcome(t, read.artifactResponse, "masked", 2, map[string]int{"github-pat": 2})
	assertNoToken(t, "owner artifact_read", toolText(t, res), tokens)

	other := mcpClient(t, srv, mintMCPToken(t, srv, "eve@stump.rocks", []string{"artifacts:read"}), nil, "other-agent")
	res = callTool(t, other, "artifact_read", map[string]any{"id": created.ID})
	raw := toolText(t, res)
	for _, k := range ownerKeys {
		if _, ok := jsonKeys(t, raw)[k]; ok {
			t.Errorf("a non-owner's artifact_read carries %q", k)
		}
	}
}

// TestRedactionWebBadgeOwnerOnly: the viewer shows the owner a badge whose
// accessible name is "2 values redacted", a keyboard-operable disclosure of
// the rule IDs, and the post-create notice in a polite live region. A
// non-owner and an anonymous reader get none of it.
func TestRedactionWebBadgeOwnerOnly(t *testing.T) {
	srv, _, logs := redactionServer(t)
	body, tokens := tokenBody()
	id := createArtifact(t, srv.URL, "markdown", "alice", body)

	resp := do(t, http.MethodGet, srv.URL+"/"+id, "alice", nil, "")
	html := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner view = %d", resp.StatusCode)
	}
	for _, frag := range []string{
		`<details class="redaction-badge" data-redaction-badge>`,
		`<summary aria-label="2 values redacted" title="2 values redacted">`,
		`<code>github-pat</code> <span class="redaction-rule-count">× 2</span>`,
		`role="status" aria-live="polite" data-redaction-notice>2 secret values in this artifact were replaced with [REDACTED] when it was created.</p>`,
		`<dd data-redaction-status>masked</dd>`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("owner view missing %q", frag)
		}
	}
	assertNoToken(t, "owner view", html, tokens)

	for _, who := range []string{"mallory", ""} {
		resp = do(t, http.MethodGet, srv.URL+"/"+id, who, nil, "")
		page := readBody(t, resp)
		if strings.Contains(page, "data-redaction") {
			t.Errorf("viewer %q sees the redaction summary", who)
		}
		assertNoToken(t, "non-owner view", page, tokens)
	}
	assertNoToken(t, "server log", logs.String(), tokens)
}

// TestRedactionWebUnscannedHasNoBadge: an owner of an unscanned artifact sees
// its status in the panel but no badge and no notice, since nothing was
// masked.
func TestRedactionWebUnscannedHasNoBadge(t *testing.T) {
	srv, st, _ := redactionServer(t)
	id := createArtifact(t, srv.URL, "markdown", "alice", "# plain")
	seedOutcome(t, st, id, redact.Summary{})
	resp := do(t, http.MethodGet, srv.URL+"/"+id, "alice", nil, "")
	html := readBody(t, resp)
	if !strings.Contains(html, `<dd data-redaction-status>unscanned</dd>`) {
		t.Error("owner view does not show the unscanned status")
	}
	if strings.Contains(html, "data-redaction-badge") || strings.Contains(html, "data-redaction-notice") {
		t.Error("an unscanned artifact shows a badge or notice")
	}
}

// TestRedactionRejectionLogCarriesNoValue: SPEC-0017 RD-9 "Logs carry no
// value", as far as this layer goes. A real reject-mode rejection rendered
// through writeError logs the rule ID, the request id, the path and the
// surface, and neither the log line nor the response holds any part of the
// token. The
// write paths that produce rejections arrive with #291-#293 and inherit this.
func TestRedactionRejectionLogCarriesNoValue(t *testing.T) {
	sc, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tok := plantedToken(7)
	_, _, rejectErr := sc.Text(context.Background(), "body", "token: "+tok+"\n", redact.ModeReject)
	if rejectErr == nil {
		t.Fatal("the scanner did not reject the planted token")
	}

	logs := &syncBuffer{}
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(logs, nil)))
	var reqID string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID = middleware.GetReqID(r.Context())
		s.writeError(w, r, rejectErr, nil)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil))

	out := logs.String()
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	for _, want := range []string{"github-pat", "request_id=" + reqID, "path=/v1/artifacts", "surface=artifact", "redaction_rules=github-pat"} {
		if reqID == "" || !strings.Contains(out, want) {
			t.Errorf("log line lacks %q: %s", want, out)
		}
	}
	body, _ := io.ReadAll(rec.Body)
	assertNoToken(t, "rejection log", out, []string{tok})
	assertNoToken(t, "rejection response", string(body), []string{tok})
}

// TestOwnerRedactionHoldsNoSecret: the owner-only JSON fields are a status, a
// flag and a count-and-rules summary, and nothing else. A new field fails the
// test until someone checks it cannot carry a value (SPEC-0017 RD-9).
func TestOwnerRedactionHoldsNoSecret(t *testing.T) {
	check := func(v any, want map[string]reflect.Type) {
		t.Helper()
		rt := reflect.TypeOf(v)
		if rt.NumField() != len(want) {
			t.Errorf("%s has %d fields, want %d", rt.Name(), rt.NumField(), len(want))
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if w, ok := want[f.Name]; !ok || f.Type != w {
				t.Errorf("%s.%s is %v, which is not an allowed outcome field", rt.Name(), f.Name, f.Type)
			}
		}
	}
	check(OwnerRedaction{}, map[string]reflect.Type{
		"RedactionStatus": reflect.TypeOf(""),
		"Redacted":        reflect.TypeOf((*bool)(nil)),
		"Redactions":      reflect.TypeOf((*redactionsView)(nil)),
	})
	check(redactionsView{}, map[string]reflect.Type{
		"Count": reflect.TypeOf(0),
		"Rules": reflect.TypeOf(map[string]int{}),
	})
	check(redactionRuleLine{}, map[string]reflect.Type{
		"Rule":  reflect.TypeOf(""),
		"Count": reflect.TypeOf(0),
	})

	// The status string is always one of the closed set.
	for _, st := range redact.Statuses {
		if got := ownerRedactionOf(redact.Summary{Status: st}).RedactionStatus; got != string(st) {
			t.Errorf("status %s renders as %q", st, got)
		}
	}
	if got := ownerRedactionOf(redact.Summary{}).RedactionStatus; got != "unscanned" {
		t.Errorf("zero outcome renders as %q, want unscanned", got)
	}
}
