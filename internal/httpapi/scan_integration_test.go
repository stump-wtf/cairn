package httpapi

// Ingest Secret Scan Transport Tests
//
// SPEC-0017 over REST and MCP: a rejection renders as a SPEC-0019
// secret_detected violation with the rule, line and column and no value; a
// bundle rejection names its member; X-Cairn-Redaction: mask (and MCP
// redaction: "mask") downgrades, and any other value is refused as
// unknown_value; a masked upload's declared checksum is accepted and the
// response carries the masked SHA-256 with redacted. Every test plants a token
// built at run time from split literals and greps the response and the log.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-5, RD-10, RD-11; ADR-0025,
// SPEC-0019 VE-1, VE-6
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

func init() { guardSiteSources = append(guardSiteSources, redactionSites) }

// redactionSites registers the create path's redaction rejections with the
// SPEC-0019 VE-6 migration guard.
func redactionSites(t *testing.T) []guardSite {
	reject := func(mode redact.Mode, body string) func() error {
		return func() error {
			_, _, err := testScanner().Text(context.Background(), "body", body, mode)
			if err == nil {
				return nil
			}
			return store.RejectionViolation(err.(*redact.Rejection))
		}
	}
	return []guardSite{
		{"redaction: X-Cairn-Redaction off", func() error {
			_, err := redactionDowngrade(redactionHeader, errs.LocHeader, "off")
			return err
		}},
		{"redaction: MCP redaction argument", func() error {
			_, err := redactionDowngrade("redaction", errs.LocBody, "none")
			return err
		}},
		{"redaction: secret detected", reject(redact.ModeReject, "key="+plantedToken(31))},
		{"redaction: too large to scan", func() error {
			return store.RejectionViolation(&redact.Rejection{Field: "body", Reason: redact.ReasonTooLargeToScan, Limit: 16 << 20, Size: 20 << 20})
		}},
	}
}

func postBody(t *testing.T, url, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func multipartFiles(t *testing.T, files ...[2]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := mw.CreateFormFile("file", f[0])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(f[1]))
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// decodeRaw returns the raw body and its decoded error envelope.
func decodeRaw(t *testing.T, resp *http.Response) (string, errorEnvelope) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	e := decodeError(t, &http.Response{Body: io.NopCloser(bytes.NewReader(raw))})
	return string(raw), e
}

// TestScanRESTRejection: SPEC-0017 RD-1 "Rejected write leaves nothing" and
// RD-11 over REST. A code artifact holding a token is refused with a
// secret_detected violation (rule, line, column, the downgrade named) and no
// value; the log names the rule and surface and not the token; the owner's Bin
// is empty.
func TestScanRESTRejection(t *testing.T) {
	srv, _, logs := redactionServer(t)
	tok := plantedToken(32)
	resp := postBody(t, srv.URL+"/v1/artifacts?type=code", "alice", []byte("package x\n\nvar k = \""+tok+"\"\n"),
		map[string]string{"Content-Type": "text/x-go"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	raw, e := decodeRaw(t, resp)
	if e.Error.Code != errs.CodeValidation || len(e.Error.Violations) != 1 {
		t.Fatalf("error = %+v", e.Error)
	}
	v := e.Error.Violations[0]
	if v.Field != "body" || v.Location != errs.LocBody || v.Reason != errs.ReasonSecretDetected || v.Value != nil {
		t.Errorf("violation = %+v", v)
	}
	for _, want := range []string{`"rule":"github-pat"`, `"line":3`, `"column":10`, "--redact=mask"} {
		if !strings.Contains(raw, want) {
			t.Errorf("response lacks %s: %s", want, raw)
		}
	}
	for _, want := range []string{"surface=artifact", "redaction_rules=github-pat", "redaction_reason=secret_detected"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %s", want)
		}
	}
	assertNoToken(t, "rejection response", raw, []string{tok})
	assertNoToken(t, "server log", logs.String(), []string{tok})

	bin := do(t, http.MethodGet, srv.URL+"/v1/bin", "alice", nil, "")
	if body := readBody(t, bin); strings.Contains(body, `"id"`) {
		t.Errorf("a rejected create is in the owner's Bin: %s", body)
	}
}

// TestScanRESTDowngrade: SPEC-0017 RD-5 over REST, raw and multipart.
// X-Cairn-Redaction: mask stores masked code; "off" is refused as
// unknown_value on the header, and nothing is stored.
func TestScanRESTDowngrade(t *testing.T) {
	srv, _, logs := redactionServer(t)
	tok := plantedToken(33)
	code := []byte("var k = \"" + tok + "\"\n")

	resp := postBody(t, srv.URL+"/v1/artifacts?type=code", "alice", code,
		map[string]string{"Content-Type": "text/plain", redactionHeader: "mask"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("downgraded create = %d", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	wantOwnerOutcome(t, art, "masked", 1, map[string]int{"github-pat": 1})

	body, ct := multipartFiles(t, [2]string{"a.go", string(code)}, [2]string{"b.txt", "fine"})
	resp = postBody(t, srv.URL+"/v1/artifacts", "alice", body, map[string]string{"Content-Type": ct, redactionHeader: "mask"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("downgraded bundle = %d", resp.StatusCode)
	}
	wantOwnerOutcome(t, decodeArtifact(t, resp), "masked", 1, map[string]int{"github-pat": 1})

	for _, send := range []func() *http.Response{
		func() *http.Response {
			return postBody(t, srv.URL+"/v1/artifacts?type=code", "alice", code,
				map[string]string{"Content-Type": "text/plain", redactionHeader: "off"})
		},
		func() *http.Response {
			return postBody(t, srv.URL+"/v1/artifacts", "alice", body, map[string]string{"Content-Type": ct, redactionHeader: "off"})
		},
	} {
		resp := send()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("X-Cairn-Redaction: off = %d, want 400", resp.StatusCode)
		}
		raw, e := decodeRaw(t, resp)
		vs := e.Error.Violations
		if len(vs) != 1 || vs[0].Field != redactionHeader || vs[0].Location != errs.LocHeader || vs[0].Reason != errs.ReasonUnknownValue || valueOf(vs[0]) != "off" {
			t.Errorf("violations = %+v", vs)
		}
		assertNoToken(t, "refusal", raw, []string{tok})
	}
	assertNoToken(t, "server log", logs.String(), []string{tok})
}

// TestScanRESTBundleRejectionNamesMember: SPEC-0017 RD-4 over multipart. The
// third file's token refuses the bundle under members[2].content, and the log
// names the bundle surface.
func TestScanRESTBundleRejectionNamesMember(t *testing.T) {
	srv, _, logs := redactionServer(t)
	tok := plantedToken(34)
	body, ct := multipartFiles(t, [2]string{"a.txt", "a"}, [2]string{"b.txt", "b"}, [2]string{"c.env", "TOKEN=" + tok + "\n"})
	resp := postBody(t, srv.URL+"/v1/artifacts", "alice", body, map[string]string{"Content-Type": ct})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	raw, e := decodeRaw(t, resp)
	if vs := e.Error.Violations; len(vs) != 1 || vs[0].Field != "members[2].content" || vs[0].Reason != errs.ReasonSecretDetected || !strings.Contains(raw, `"rule":"github-pat"`) {
		t.Errorf("violations = %+v (%s)", vs, raw)
	}
	if !strings.Contains(logs.String(), "surface=bundle") {
		t.Error("the log does not name the bundle surface")
	}
	assertNoToken(t, "response", raw, []string{tok})
	assertNoToken(t, "server log", logs.String(), []string{tok})
}

// TestScanRESTDeclaredChecksumOfMaskedUpload: SPEC-0017 RD-10 "Declared
// checksum of a masked upload". The raw body's declared SHA-256 is accepted,
// and the response carries the masked body's SHA-256 and redacted: true.
func TestScanRESTDeclaredChecksumOfMaskedUpload(t *testing.T) {
	srv, _, _ := redactionServer(t)
	tok := plantedToken(35)
	raw := "# notes\n\ntoken " + tok + "\n"
	sum := sha256.Sum256([]byte(raw))
	resp := postBody(t, srv.URL+"/v1/artifacts?type=markdown", "alice", []byte(raw),
		map[string]string{"Content-Type": "text/markdown", "X-Cairn-Sha256": hex.EncodeToString(sum[:])})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	masked := sha256.Sum256([]byte(strings.Replace(raw, tok, redact.Mask, 1)))
	if art.Checksum != hex.EncodeToString(masked[:]) || art.Redacted == nil || !*art.Redacted {
		t.Errorf("checksum %s redacted %v, want the masked body's and true", art.Checksum, art.Redacted)
	}
	got := readBody(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID+"/body", "alice", nil, ""))
	if got != strings.Replace(raw, tok, redact.Mask, 1) {
		t.Errorf("stored body = %q", got)
	}
}

// TestScanMCPCreate: SPEC-0017 RD-5 and RD-4 over MCP. artifact_create
// refuses a code body with a token, masks it with redaction "mask", and
// refuses any other redaction value; bundle_create refuses a member with a
// token and masks it with the downgrade. No output or log line holds the token.
func TestScanMCPCreate(t *testing.T) {
	logs := &syncBuffer{}
	srv, _ := mcpTestServerLogs(t, logs)
	owner := mcpClient(t, srv, mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"}), nil, "claude-desktop")
	tok := plantedToken(36)
	code := "var k = \"" + tok + "\"\n"

	res := callTool(t, owner, "artifact_create", map[string]any{"body": code, "share_type": "code"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "validation_failed:") {
		t.Errorf("code with a token: IsError=%v %q, want validation_failed", res.IsError, toolText(t, res))
	}
	assertNoToken(t, "artifact_create refusal", toolText(t, res), []string{tok})

	res = callTool(t, owner, "artifact_create", map[string]any{"body": code, "share_type": "code", "redaction": "mask"})
	var created mcpCreateOutput
	decodeToolJSON(t, res, &created)
	wantOwnerOutcome(t, created.artifactResponse, "masked", 1, map[string]int{"github-pat": 1})
	assertNoToken(t, "artifact_create output", toolText(t, res), []string{tok})

	res = callTool(t, owner, "artifact_create", map[string]any{"body": "fine", "redaction": "off"})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "validation_failed:") {
		t.Errorf("redaction off: IsError=%v %q, want validation_failed", res.IsError, toolText(t, res))
	}

	members := []map[string]any{{"name": "a.txt", "body": "a"}, {"name": "b.env", "body": "TOKEN=" + tok}}
	res = callTool(t, owner, "bundle_create", map[string]any{"members": members})
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "validation_failed:") {
		t.Errorf("bundle with a token: IsError=%v %q, want validation_failed", res.IsError, toolText(t, res))
	}
	res = callTool(t, owner, "bundle_create", map[string]any{"members": members, "redaction": "mask"})
	var bundle mcpBundleCreateOutput
	decodeToolJSON(t, res, &bundle)
	wantOwnerOutcome(t, bundle.artifactResponse, "masked", 1, map[string]int{"github-pat": 1})

	if out := logs.String(); !strings.Contains(out, "surface=bundle") || !strings.Contains(out, "surface=artifact") {
		t.Errorf("the MCP rejections are not logged with their surface: %s", out)
	}
	assertNoToken(t, "server log", logs.String(), []string{tok})
}

// mcpTestServerLogs is mcpTestServer with the log output in reach.
func mcpTestServerLogs(t *testing.T, logs io.Writer) (*httptest.Server, *store.Store) {
	t.Helper()
	st := store.New(newTestPool(t), objectstore.NewMemory(), withScanner(store.Options{}))
	srv := httptest.NewServer(New(st, nil, nil, mcpConfig(), slog.New(slog.NewTextHandler(logs, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}
